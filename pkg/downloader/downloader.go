package downloader

import (
	"compress/flate"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	netUrl "net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/jlaffaye/ftp"
	"github.com/lxn/win"
	"github.com/mzky/weblink/internal/log"
)

type Downloader struct {
	lastJobId uint64
	Option

	afterCreateJobInterceptor AfterCreateJobInterceptor
}

type Job struct {
	downloader *Downloader
	Option

	id             uint64
	Url            *netUrl.URL
	FileName       string
	FileSize       uint64
	isSupportRange bool
	isFtp          bool

	_lck *sync.Mutex // 用于确保写入文件的顺序
}

type Option struct {
	Dir                  string         // 下载路径，如果为空则使用当前目录
	FileNamePrefix       string         // 文件名前缀，默认空
	MaxThreads           int            // 下载线程，默认4
	MinChunkSize         uint64         // 最小分块大小，默认500KB
	EnableSaveFileDialog bool           // 是否打开保存文件对话框，默认true
	Overwrite            bool           // 是否覆盖已存在的文件，默认false
	Timeout              time.Duration  // 超时时间，默认10秒
	Cookies              []*http.Cookie // Cookie
}

type AfterCreateJobInterceptor func(job *Job)

func (opt Option) cloneOption() Option {
	return Option{
		Dir:                  opt.Dir,
		FileNamePrefix:       opt.FileNamePrefix,
		MaxThreads:           opt.MaxThreads,
		MinChunkSize:         opt.MinChunkSize,
		EnableSaveFileDialog: opt.EnableSaveFileDialog,
		Overwrite:            opt.Overwrite,
		Timeout:              opt.Timeout,
	}
}

func New(withOption ...func(*Option)) *Downloader {

	pwd, err := os.Getwd()
	if err != nil {
		pwd = ""
	}

	// 默认参数
	opt := Option{
		Dir:                  pwd,
		FileNamePrefix:       "",
		MaxThreads:           4,
		MinChunkSize:         500 * 1024, // 500KB
		EnableSaveFileDialog: true,
		Overwrite:            false,
		Timeout:              10 * time.Second,
	}

	for _, set := range withOption {
		set(&opt)
	}

	downloader := &Downloader{
		lastJobId: 0,
		Option:    opt,
	}

	// 空实现
	downloader.afterCreateJobInterceptor = func(job *Job) {}

	return downloader
}

func (d *Downloader) Download(url string, withOption ...func(*Option)) error {
	job, err := d.NewJob(url, withOption...)
	if err != nil {
		return err
	}
	return job.Download()
}

func (d *Downloader) DownloadFile(url string, withOption ...func(*Option)) error {
	job, err := d.NewJob(url, withOption...)
	if err != nil {
		return err
	}
	return job.DownloadFile()
}
func (d *Downloader) NewJob(url string, withOption ...func(*Option)) (*Job, error) {

	Url, err := netUrl.Parse(url)
	if err != nil {
		return nil, err
	}

	d.lastJobId++

	opt := d.Option.cloneOption()

	for _, set := range withOption {
		set(&opt)
	}

	job := &Job{
		downloader: d,
		Option:     opt,

		id:             d.lastJobId,
		Url:            Url,
		FileName:       "",
		FileSize:       0,
		isSupportRange: false,
		isFtp:          Url.Scheme == "ftp",
	}

	d.afterCreateJobInterceptor(job)

	return job, nil
}

func (d *Downloader) AfterCreateJob(interceptor AfterCreateJobInterceptor) {
	d.afterCreateJobInterceptor = interceptor
}

func (job *Job) TargetFile() string {

	if filepath.IsAbs(job.FileName) {
		return job.FileName
	}

	return filepath.Join(job.Dir, job.FileNamePrefix+job.FileName)
}

func (job *Job) createTargetFile() (*os.File, error) {

	if job.Overwrite {
		return os.Create(job.TargetFile())
	}

	original := job.TargetFile()
	// 检查文件是否存在
	if _, err := os.Stat(original); os.IsNotExist(err) {
		// 文件不存在，返回新建文件
		return os.Create(original)
	}

	index := 1

	base := job.FileNamePrefix + job.FileName
	ext := filepath.Ext(base)
	baseWithoutExt := base[:len(base)-len(ext)]

	for {
		// 构造新的文件名
		newBase := fmt.Sprintf("%s(%d)%s", baseWithoutExt, index, ext)
		newPath := filepath.Join(job.Dir, newBase)

		// 检查文件是否存在
		if _, err := os.Stat(newPath); os.IsNotExist(err) {

			job.FileName = strings.TrimPrefix(newBase, job.FileNamePrefix)

			// 文件不存在，返回新建文件
			return os.Create(job.TargetFile())
		}

		// 文件存在，增加索引并重试
		index++
	}

}

func (job *Job) AvaiableTreads() int {

	// 不支持多线程下载
	if !job.isSupportRange {
		return 1
	}

	if job.FileSize < job.MinChunkSize {
		return 1
	}

	// 如果最小分块大小错误，则单线程下载
	if job.MinChunkSize <= 0 {
		return 1
	}

	threads := int(math.Ceil(float64(job.FileSize) / float64(job.MinChunkSize)))

	if threads > job.MaxThreads {
		return job.MaxThreads
	}

	if threads < 1 {
		return 1
	}

	return threads
}

func (job *Job) Download() error {
	select {
	case <-time.After(job.Timeout):
		return errors.New("下载超时")
	default:
		if job.isFtp {
			return job.downloadFtp()
		}

		return job.downloadHttp()
	}
}

func (job *Job) downloadFtp() error {

	c, err := ftp.Dial(job.Url.Host+job.Url.Port(), ftp.DialWithTimeout(5*time.Second))
	if err != nil {
		job.logErr("打开 FTP 出错：" + err.Error())
		return errors.New("链接 FTP 服务器出错")
	}

	username := job.Url.User.Username()
	password, _ := job.Url.User.Password()

	if username == "" {
		username = "anonymous"
	}

	err = c.Login(username, password)
	defer c.Quit()
	if err != nil {
		job.logErr("登录 FTP 出错：" + err.Error())
		return errors.New("登录 FTP 出错")
	}

	if job.EnableSaveFileDialog {
		if path, ok := openSaveFileDialog(job.TargetFile()); ok {
			dir, file := filepath.Split(path)
			job.FileName = file
			job.Dir = dir
		} else {
			job.logDebug("用户取消保存。")
			return nil
		}
	} else {
		job.FileName = filepath.Base(job.Url.Path)
	}

	if job.FileName == "" || !strings.Contains(job.FileName, ".") {
		job.logDebug("文件名不正确: %s", job.FileName)
		return errors.New("文件名不正确。")
	}

	job.logDebug("创建任务 %s", job.Url.String())

	r, err := c.Retr(job.Url.Path)
	if err != nil {
		return err
	}
	defer r.Close()

	// 打开文件准备写入
	file, err := job.createTargetFile()
	if err != nil {
		return err
	}
	defer file.Close()

	buf, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	_, err = file.Write(buf)

	return err
}

func (job *Job) downloadHttp() error {
	if err := job.fetchInfo(); err != nil {
		job.logErr("获取文件信息出错：" + err.Error())
		return err
	}

	job.logDebug("创建任务 %s", job.Url)
	if job.EnableSaveFileDialog {
		if path, ok := openSaveFileDialog(job.TargetFile()); ok {
			dir, file := filepath.Split(path)
			job.FileName = file
			job.Dir = dir
		} else {
			job.logDebug("用户取消保存。")
			return nil
		}
	}

	if job.FileName == "" || !strings.Contains(job.FileName, ".") {
		job.logDebug("文件名不正确: %s", job.FileName)
		return errors.New("文件名不正确。")
	}

	if err := job.multiThreadDownload(); err != nil {
		job.logErr(err.Error())
		return err
	}

	job.logDebug("下载完成：%s", job.TargetFile())
	return nil
}

func (job *Job) multiThreadDownload() error {

	theads := job.AvaiableTreads()
	job.logDebug("文件将以多线程进行下载，线程：%d", theads)

	// 打开文件准备写入
	file, err := job.createTargetFile()
	if err != nil {
		return err
	}
	defer file.Close()

	var wg sync.WaitGroup
	var ctx, cancel = context.WithCancel(context.Background())
	var errs []error
	var errLock sync.Mutex

	defer cancel() // 取消所有goroutine

	// 计算每个线程的分块大小
	chunkSize := uint64(math.Ceil(float64(job.FileSize) / float64(theads)))

	for i := 0; i < theads; i++ {
		start := uint64(i) * chunkSize
		end := start + chunkSize - 1

		// 如果是最后一个部分，加上余数
		if i == theads-1 {
			end = job.FileSize - 1
		}

		wg.Add(1)

		go func(index int) {
			defer wg.Done()

			retry := 0
			for {
				select {
				case <-ctx.Done():
					// 如果收到取消信号，直接返回
					return
				default:
					// 尝试下载分块
					err := job.downloadChunk(file, start, end)

					if err == nil {
						job.logDebug("切片 %d 下载完成", index+1)
						return
					}

					// 如果重试超过3次，记录错误并触发取消操作
					if retry >= 3 {
						errLock.Lock()
						errs = append(errs, err)
						errLock.Unlock()
						cancel() // 取消所有goroutine
						return
					}

					retry++
				}
			}
		}(i)
	}

	wg.Wait() // 等待所有goroutine完成

	if len(errs) > 0 {
		return errs[0] // 返回第一个遇到的错误
	}

	return nil
}

// downloadChunk 下载文件的单个分块
func (job *Job) downloadChunk(file *os.File, start, end uint64) error {
	req, err := http.NewRequest("GET", job.Url.String(), nil)
	if err != nil {
		return err
	}

	for _, cookie := range job.Option.Cookies {
		req.AddCookie(cookie)
	}
	// 设置Range头实现断点续传
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 检查服务器是否支持Range请求
	if resp.StatusCode != http.StatusPartialContent {
		return errors.New("server doesn't support Range requests")
	}

	// 锁定互斥锁以安全地写入文件
	job._lck.Lock()
	defer job._lck.Unlock()

	// 写入文件的当前位置
	if _, err = file.Seek(int64(start), io.SeekStart); err != nil {
		return err
	}

	// 将HTTP响应的Body内容写入到文件中
	_, err = io.Copy(file, resp.Body)
	return err
}

func (job *Job) fetchInfo() error {
	// 创建一个 HTTP 头请求
	req, err := http.NewRequest("HEAD", job.Url.String(), nil)
	if err != nil {
		return err
	}

	for _, cookie := range job.Option.Cookies {
		req.AddCookie(cookie)
	}

	r, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer r.Body.Close()

	if r.StatusCode == 404 {
		return fmt.Errorf("文件不存在：%s", job.Url.String())
	}

	if r.StatusCode == 401 {
		return fmt.Errorf("无权访问：%s", job.Url.String())
	}

	if r.StatusCode > 299 {
		return fmt.Errorf("连接 %s 出错。", job.Url.String())
	}

	// 检查是否支持 断点续传
	// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Accept-Ranges
	if r.Header.Get("Accept-Ranges") == "bytes" {
		job.isSupportRange = true
	}

	// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Content-Length
	// 获取文件总大小 #有些连接无法获取文件大小
	contentLength, err := strconv.ParseUint(r.Header.Get("Content-Length"), 10, 64)
	if err != nil {
		job.isSupportRange = false
		job.FileSize = 0
		return nil
	}
	job.FileSize = contentLength

	if !job.EnableSaveFileDialog {
		job.FileName = getFileNameByResponse(r)
	}

	return nil
}

func getFileNameByResponse(resp *http.Response) string {
	contentDisposition := resp.Header.Get("Content-Disposition")
	if contentDisposition != "" {
		_, params, err := mime.ParseMediaType(contentDisposition)

		if err != nil {
			return getFileNameByUrl(resp.Request.URL.Path)
		}
		return params["FileName"]
	}
	return getFileNameByUrl(resp.Request.URL.Path)
}

func getFileNameByUrl(downloadUrl string) string {
	parsedUrl, _ := netUrl.Parse(downloadUrl)
	return filepath.Base(parsedUrl.Path)
}

func openSaveFileDialog(fileName string) (filePath string, ok bool) {
	var ofn win.OPENFILENAME
	buf := make([]uint16, syscall.MAX_PATH)
	ofn.LStructSize = uint32(unsafe.Sizeof(ofn))
	ofn.NMaxFile = uint32(len(buf))
	ofn.LpstrFile, _ = syscall.UTF16PtrFromString(fileName)
	ofn.Flags = win.OFN_OVERWRITEPROMPT | win.OFN_EXPLORER | win.OFN_FILEMUSTEXIST | win.OFN_PATHMUSTEXIST | win.OFN_LONGNAMES
	ofn.LpstrTitle, _ = syscall.UTF16PtrFromString("保存文件")
	ofn.LpstrFilter, _ = syscall.UTF16PtrFromString("All Files (*.*)")
	ok = win.GetSaveFileName(&ofn)
	if ok {
		filePath = syscall.UTF16ToString((*[1 << 16]uint16)(unsafe.Pointer(ofn.LpstrFile))[:])
	}
	return
}

func (job *Job) logDebug(tpl string, vars ...interface{}) {
	log.Debug(fmt.Sprintf("[下载任务 %d ]: ", job.id)+tpl, vars...)
}

func (job *Job) logErr(tpl string, vars ...interface{}) {

	log.Error(fmt.Sprintf("[下载任务 %d ]: ", job.id)+tpl, vars...)
}

// 获取下载的文件名
func getFileName(resp *http.Response) string {
	contentDisposition := resp.Header.Get("Content-Disposition")
	if contentDisposition != "" {
		fn := strings.Split(contentDisposition, "=")
		if len(fn) > 1 {
			return fn[1]
		}
	}
	return ""
}

func (job *Job) DownloadFile() error {
	// 创建一个 HTTP 头请求
	req, err := http.NewRequest("GET", job.Url.String(), nil)
	if err != nil {
		return err
	}

	for _, cookie := range job.Option.Cookies {
		req.AddCookie(cookie)
	}

	r, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer r.Body.Close()

	job.FileName = getFileName(r)

	// 创建本地文件
	path, ok := openSaveFileDialog(job.TargetFile())
	if !ok {
		return fmt.Errorf("请选择保存文件位置")
	}
	localFile, err := os.Create(path)
	if err != nil {
		return err
	}
	defer localFile.Close()

	var reader io.Reader
	// 检查Content-Encoding是否为deflate
	contentEncoding := r.Header.Get("Content-Encoding")
	if contentEncoding == "deflate" {
		// 如果是deflate编码，解压缩数据
		reader = flate.NewReader(r.Body)
	} else {
		// 如果不是deflate编码，直接将响应体内容写入文件
		reader = r.Body
	}
	if _, e := io.Copy(localFile, reader); e != nil {
		return e
	}
	// 检查文件是否成功写入
	return localFile.Sync()
}
