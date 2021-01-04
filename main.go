package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpproxy"
	_ "golang.org/x/image/webp"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
)

type Conf struct {
	SavePath  string
	Socks     string
	Retry     int
	ThreadNum int
}

type Gallery struct {
	Id      string  `json:"id"`
	Title   string  `json:"title"`
	JpTitle string  `json:"japanese_title"`
	Lang    string  `json:"language"`
	Files   []Image `json:"files"`
	Url     string
}

type Image struct {
	Name    string `json:"name"`
	Hash    string `json:"hash"`
	HasWebp int    `json:"haswebp"`
	HasAvif int    `json:"hasavif"`
}

type Job struct {
	Image    Image
	Gallery  Gallery
	SavePath string
	Conf     Conf
}

type WriteJob struct {
	Content  []byte
	FileName string
}

var conf Conf
var Client fasthttp.Client
var downloadingCount int64
var downloadStartCount int64

var queue chan Job
var galleryQueue chan Gallery
var writeQueue chan WriteJob

func main() {
	confByte, err := ioutil.ReadFile("./config.json")
	if err != nil {
		log.Fatal(err)
	}
	if err = json.Unmarshal(confByte, &conf); err != nil {
		log.Fatal(err)
	}
	if conf.ThreadNum < 1 || conf.ThreadNum > runtime.NumCPU() {
		conf.ThreadNum = runtime.NumCPU()
	}
	list, err := ioutil.ReadFile("list.txt")
	if err != nil {
		if os.IsNotExist(err) {
			log.Fatal("list.txt Not Found")
		}
		log.Fatal(err)
	}
	listStr := strings.TrimSpace(string(list))
	if listStr == "" {
		log.Fatal("Empty List")
	}
	galleryUrls := Unique(strings.Split(listStr, Eol()))
	if conf.Socks != "" {
		Client.Dial = fasthttpproxy.FasthttpSocksDialer(conf.Socks)
	}
	queue = make(chan Job, conf.ThreadNum)
	galleryQueue = make(chan Gallery, conf.ThreadNum)
	writeQueue = make(chan WriteJob, conf.ThreadNum*100)
	runtime.GOMAXPROCS(conf.ThreadNum)

	for i := 0; i < conf.ThreadNum; i++ {
		go func() {
			DownloadImageWorker()
		}()
	}

	go func() {
		WriteWorker()
	}()

	go func() {
		for _, url := range galleryUrls {
			gallery, err := GalleryInfo(url)
			if err != nil {
				log.Println("Read Gallery Info Fail: " + url + " Because " + err.Error())
			} else {
				gallery.Url = url
				galleryQueue <- gallery
			}
		}
	}()

	for i := 0; ; i++ {
		gallery := <-galleryQueue
		DownloadGallery(gallery, i, len(galleryUrls), conf)
		if gallery.Url == galleryUrls[i] {
			break
		}
	}

	for {
		if downloadingCount == 0 {
			fmt.Println()
			log.Println("Download Finish")
			break
		}
	}
	_, _ = fmt.Scanf("wait")
}

func DownloadGallery(gallery Gallery, index int, total int, conf Conf) {
	lang := gallery.Lang
	title := gallery.JpTitle
	if lang == "" {
		lang = "null"
	}
	if title == "" {
		title = gallery.Title
	}
	fmt.Println()
	log.Println("Start Download (" + strconv.Itoa(index+1) + "/" + strconv.Itoa(total) + "): " + title)
	savePath := conf.SavePath + lang + "/" + ValidFileName(title)
	if err := os.MkdirAll(savePath, os.ModeDir); err != nil {
		if os.IsExist(err) {
			savePath = conf.SavePath + lang + "/" + ValidFileName(title) + " - " + gallery.Id
			if err := os.MkdirAll(savePath, os.ModeDir); err != nil {
				log.Print(err)
				return
			}
		}
		log.Print(err)
		return
	}
	for _, img := range gallery.Files {
		job := Job{
			Image:    img,
			Gallery:  gallery,
			SavePath: savePath,
			Conf:     conf,
		}
		queue <- job
	}
}

func DownloadImageWorker() {
	for {
		select {
		case job := <-queue:
			DownloadImageHandler(job)
		default:
		}
		if downloadingCount == 0 && downloadStartCount > 0 {
			break
		}
	}
}

func DownloadImageHandler(job Job) {
	fmt.Print(".")
	atomic.AddInt64(&downloadingCount, 1)
	atomic.AddInt64(&downloadStartCount, 1)
	for tries := 1; ; tries++ {
		req := fasthttp.AcquireRequest()
		req.URI().Update(ImageUrl(job.Image))
		req.Header.SetMethod("GET")
		req.Header.Set("Referer", "https://hitomi.la/reader/"+job.Gallery.Id+".html")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 6.1; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/87.0.4280.88 Safari/537.36")
		res := fasthttp.AcquireResponse()
		if err := Client.Do(req, res); err == nil && res.Header.StatusCode() == 200 && res.Header.ContentLength() > 0 {
			fileName := job.Image.Name
			if job.Image.HasAvif == 1 {
				fileName = strings.Split(fileName, ".")[0] + ".avif"
			} else if job.Image.HasWebp == 1 {
				fileName = strings.Split(fileName, ".")[0] + ".webp"
			}
			writeJob := WriteJob{
				Content:  res.Body(),
				FileName: job.SavePath + "/" + fileName,
			}
			writeQueue <- writeJob
			fasthttp.ReleaseResponse(res)
			fasthttp.ReleaseRequest(req)
			break
		} else {
			if tries > conf.Retry {
				toPrint := "Download Image Fail: " + job.Image.Name + " Because Max Retry Times Reached"
				if err != nil {
					toPrint = toPrint + Eol() + "Last Error: " + err.Error()
				}
				if res.Header.StatusCode() != 200 {
					toPrint = toPrint + Eol() + "Last Error: Status Code " + strconv.Itoa(res.Header.StatusCode())
				}
				log.Println(toPrint)
				atomic.AddInt64(&downloadingCount, -1)
				break
			}
			continue
		}
	}
}

func WriteWorker() {
	for {
		select {
		case job := <-writeQueue:
			{
				WriterHandler(job)
			}
		default:
		}
		if downloadingCount == 0 && downloadStartCount > 0 {
			break
		}
	}
}

func WriterHandler(job WriteJob) {
	if err := ioutil.WriteFile(job.FileName, job.Content, os.ModeAppend); err != nil {
		log.Print("Download Image Fail: " + job.FileName + " Because " + err.Error())
	}
	atomic.AddInt64(&downloadingCount, -1)
}

func GalleryInfo(url string) (gallery Gallery, err error) {
	pieces := strings.Split(url, "-")
	last := pieces[len(pieces)-1]
	id := strings.Split(last, ".")[0]
	code, resp, err := Client.Get(nil, "https://ltn.hitomi.la/galleries/"+id+".js")
	if err != nil {
		return gallery, err
	}
	if code != 200 {
		return gallery, errors.New(strconv.Itoa(code))
	}
	resp = bytes.ReplaceAll(resp, []byte("var galleryinfo = "), []byte(""))
	err = json.Unmarshal(resp, &gallery)
	if err != nil {
		return gallery, err
	}
	return gallery, nil
}

func ImageUrl(img Image) string {
	var retval string
	subDomain := "a"
	directory := "images"
	var numberOfFrontends int64 = 3

	h1 := img.Hash[len(img.Hash)-1:]
	h2 := img.Hash[len(img.Hash)-3 : len(img.Hash)-1]
	ext := filepath.Ext(img.Name)

	if img.HasAvif == 1 {
		directory = "avif"
		ext = ".avif"
		retval = "a"
	} else if img.HasWebp == 1 {
		directory = "webp"
		ext = ".webp"
		retval = "a"
	} else {
		retval = "b"
	}

	g, err := strconv.ParseInt(h2, 16, 64)
	if err == nil {
		if g < 0x30 {
			numberOfFrontends = 2
		}
		if g < 0x09 {
			g = 1
		}
		char := 97 + g%numberOfFrontends
		subDomain = string(char) + retval
	}

	return "https://" + subDomain + ".hitomi.la/" + directory + "/" + h1 + "/" + h2 + "/" + img.Hash + ext
}

func Unique(strSlice []string) []string {
	keys := make(map[string]struct{})
	list := make([]string, 0)
	for _, str := range strSlice {
		if _, exists := keys[str]; !exists {
			keys[str] = struct{}{}
			list = append(list, str)
		}
	}
	return list
}

func ValidFileName(path string) string {
	illegalChars := []string{":", "/", "\\", "?", "*", "\"", "<", ">", "|"}
	for _, illegalChar := range illegalChars {
		path = strings.ReplaceAll(path, illegalChar, "")
	}
	return path
}

func Eol() string {
	if runtime.GOOS == "windows" {
		return "\r\n"
	} else {
		return "\n"
	}
}
