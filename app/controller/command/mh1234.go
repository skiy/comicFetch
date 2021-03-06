package command

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"github.com/gogf/gf/crypto/gmd5"
	"github.com/gogf/gf/frame/g"
	"github.com/gogf/gf/os/gfile"
	"github.com/skiy/comic-fetch/app/config"
	"github.com/skiy/comic-fetch/app/library/lcfg"
	"github.com/skiy/comic-fetch/app/library/ldb"
	"github.com/skiy/comic-fetch/app/library/lfetch"
	"github.com/skiy/comic-fetch/app/library/lfilepath"
	"github.com/skiy/comic-fetch/app/library/llog"
	"github.com/skiy/comic-fetch/app/library/lnotify"
	"github.com/skiy/comic-fetch/app/library/lstrings"
	"github.com/skiy/comic-fetch/app/model"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Mh1234 漫画1234
type Mh1234 struct {
	Books    *model.TbBooks
	WebURL   string
	ResURL   string
	Notified bool
}

// AddBook Add new comic
func (t *Mh1234) AddBook(siteURL string) (err error) {
	t.WebURL = siteURL

	t.Books.OriginURL = fmt.Sprintf("%s/wap/comic/%d.html", t.WebURL, t.Books.OriginBookID)

	if err = t.ToFetchBook(); err != nil {
		return err
	}

	timeNow := time.Now().Unix()
	t.Books.UpdatedAt = timeNow
	t.Books.CreatedAt = timeNow

	bookModel := model.NewBooks()
	bookRes, err := bookModel.AddData(t.Books)
	if err != nil {
		return err
	}

	cfg := lcfg.GetCfg()
	notifyType := cfg.GetInt("notify.type")
	notifyNewBook := cfg.GetBool("notify.book")

	if notifyNewBook {
		notify := lnotify.NewNotifyMessage(1)

		// 钉钉通知
		if notifyType == 1 {
			if err := notify.Dingtalk(t.Books.Name, t.Books.OriginWeb, t.Books.OriginImageURL, t.Books.OriginURL); err != nil {
				llog.Log.Warningf("新增漫画通知失败: %v", err)
			}

			t.Notified = true
		}
	}

	t.Books.ID, _ = bookRes.LastInsertId()

	return t.ToFetch()
}

// ToFetch 采集
func (t *Mh1234) ToFetch() (err error) {
	log := llog.Log

	web, ok := config.WebURL[t.Books.OriginFlag]
	if ok {
		t.WebURL = web[t.Books.OriginWebType]
	} else {
		return errors.New("index out of range for origin_web_type: " + t.Books.OriginFlag)
	}

	if t.WebURL == "" {
		return errors.New("WebURL is nil: " + t.Books.OriginFlag)
	}

	// 采集章节列表
	chapterURLList, err := t.ToFetchChapterList()
	if err != nil {
		return err
	}

	if len(chapterURLList) == 0 {
		return errors.New("获取不到章节数据")
	}

	// 从数据库中获取已采集的章节列表
	chapterRes, err := ldb.GetDB().Table(config.TbNameChapters).Where(g.Map{"book_id": t.Books.ID}).Select()
	if err != nil {
		if err != sql.ErrNoRows {
			return err
		}
		err = nil
	}

	var chapters []model.TbChapters
	chapterStatusMap := map[int]model.TbChapters{}

	if chapterRes != nil {
		if err := chapterRes.ToStructs(&chapters); err != nil {
			return err
		}

		// 章节转 Map
		for _, chapter := range chapters {
			//log.Println(chapter)
			chapterStatusMap[chapter.OriginID] = chapter
		}
	}

	orderID := len(chapters) + 1
	cfg := lcfg.GetCfg()

	imageLocal := cfg.GetBool("image.local")
	filePath := cfg.GetString("image.path")
	nametype := cfg.GetString("image.nametype")

	// 这里应该用 channel 并发获取章节数据
	for _, chapterURL := range chapterURLList {
		preg := `\/wap\/comic\/[0-9]*/([0-9]*).html`
		re, err := regexp.Compile(preg)
		if err != nil {
			log.Warningf("章节ID正则执行失败: %v, URL: %s", err, chapterURL)
			continue
		}
		chapterIDs := re.FindStringSubmatch(chapterURL)
		if len(chapterIDs) != 2 {
			log.Warningf("章节ID提取失败: %v, URL: %s", err, chapterURL)
			continue
		}
		chapterOriginIDStr := chapterIDs[1]
		chapterOriginID, err := strconv.Atoi(chapterOriginIDStr)
		if err != nil {
			log.Warningf("章节ID(%s)转Int型失败: %v", chapterOriginIDStr, err)
			continue
		}

		// 章节是否存在
		chapterInfo, ok := chapterStatusMap[chapterOriginID]
		// 章节已存在
		if ok {
			// 章节采集已成功 或 章节停止采集
			if chapterInfo.Status == 0 || chapterInfo.Status == 2 {
				continue
			}
		}

		fullChapterURL := t.WebURL + chapterURL
		log.Debugf("\n[URL] %s", fullChapterURL)

		chapterName, imageURLList, err := t.ToFetchChapter(fullChapterURL)
		if err != nil {
			log.Warningf("章节: %s, 图片抓取失败: %v", fullChapterURL, err)
			continue
		}

		if len(imageURLList) == 0 {
			log.Warningf("章节: %s, URL: %s, 无图片资源", chapterName, fullChapterURL)
			continue
		}

		var episodeID int
		preg2 := `^([0-9]*)`
		re2 := regexp.MustCompile(preg2)
		episodeIDs := re2.FindStringSubmatch(chapterName)
		if len(episodeIDs) < 2 || episodeIDs[1] == "" {
			preg2 := `第([0-9]*)[话章]`
			re2 := regexp.MustCompile(preg2)
			episodeIDs = re2.FindStringSubmatch(chapterName)
		}

		if len(episodeIDs) > 1 {
			episodeID, _ = strconv.Atoi(strings.Trim(episodeIDs[1], ""))
		}

		log.Debugf("[Title] %s, [Image Count] %d", chapterName, len(imageURLList))

		status := 1 // 默认失败状态
		timestamp := time.Now().Unix()

		if !ok { // 新章节
			chapter := model.TbChapters{}
			chapter.BookID = t.Books.ID
			chapter.EpisodeID = episodeID
			chapter.Title = chapterName
			chapter.OrderID = orderID
			chapter.OriginID = chapterOriginID
			chapter.Status = status
			chapter.OriginURL = fullChapterURL
			chapter.CreatedAt = timestamp
			chapter.UpdatedAt = timestamp

			if res, err := ldb.GetDB().Table(config.TbNameChapters).Data(chapter).Insert(); err != nil {
				log.Warningf("新章节: %s, URL: %s, 保存失败", chapterName, fullChapterURL)
			} else {
				orderID++
				chapterInfo.ID, _ = res.LastInsertId()

				// 未通知过
				if !t.Notified {
					cfg := lcfg.GetCfg()
					notifyType := cfg.GetInt("notify.type")
					notifyNewBook := cfg.GetBool("notify.book")

					if notifyNewBook {
						notify := lnotify.NewNotifyMessage(2)

						// 钉钉通知
						if notifyType == 1 {
							if err := notify.Dingtalk(t.Books.Name, chapter.Title, t.Books.OriginImageURL, chapter.OriginURL); err != nil {
								log.Warningf("更新漫画通知失败: %v", err)
							}

							t.Notified = true
						}
					}
				}
			}
		}

		imageModel := model.NewImages()

		var imageDataArr []model.TbImages
		for index, imageOriginURL := range imageURLList {
			imageOrderID := index + 1
			fullImageOriginURL := t.ResURL + imageOriginURL

			log.Debugf("[IMAGE URL] %s", fullImageOriginURL)

			var imageSize int64
			var imageURL string
			isRemote := 1

			// 图片本地化
			if imageLocal {
				if res, err := lfetch.GetResponse(fullImageOriginURL, fullChapterURL); err != nil {
					log.Warningf("远程获取图片本地化失败: %v", err)
				} else {
					fileName := fmt.Sprintf("%d-%d-%d", t.Books.ID, chapterInfo.ID, imageOrderID)
					if strings.EqualFold(nametype, "md5") {
						if name, err := gmd5.Encrypt(fileName); err != nil {
							log.Warningf("图片本地化文件名 MD5 加密失败: %v", err)
						} else {
							fileName = name
						}
					}

					fileExt := lfilepath.Ext(fullImageOriginURL)
					if fileExt == "" {
						fileExt = ".jpg"
					}

					// 真实保存的文件名
					fullFileName := fmt.Sprintf("%s/%s%s", filePath, fileName, fileExt)

					if imageFile, err := gfile.Create(fullFileName); err != nil {
						log.Warningf("本地化图片创建失败: %v", err)
					} else { // 创建成功
						imageSize, err = io.Copy(imageFile, res.Response.Body)
						if err != nil {
							log.Warningf("本地化图片保存失败: %v", err)
							if err := os.Remove(fullFileName); err != nil {
								log.Warningf("本地文件(%s)删除失败: %v", fullFileName, err)
							}
						} else { // 图片保存到本地成功
							isRemote = 0
							imageURL = fullFileName
						}
					}
				}
			}

			imageDataArr = append(imageDataArr, model.TbImages{
				0,
				t.Books.ID,
				chapterInfo.ID,
				episodeID,
				imageURL,
				fullImageOriginURL,
				imageSize,
				imageOrderID,
				isRemote,
				timestamp,
			})
		}

		//break
		// 保存图片数据
		if _, err := imageModel.AddDataBatch(imageDataArr, 0); err != nil {
			log.Warningf("图片批量保存到数据库失败: %v", err)
		} else { // 图片保存成功
			chapterData := g.Map{
				"status":     0,
				"updated_at": time.Now().Unix(),
			}

			if _, err := ldb.GetDB().Table(config.TbNameChapters).Data(chapterData).Where(g.Map{"id": chapterInfo.ID}).Update(); err != nil {
				log.Warningf("章节(%d): %s, URL: %s, 状态(0)更新失败", chapterInfo.ID, chapterName, fullChapterURL)
			}
		}

		//break
	}

	return
}

// ToFetchBook 获取漫画信息
func (t *Mh1234) ToFetchBook() (err error) {
	doc, err := lfetch.PageSource(t.Books.OriginURL, "utf-8")
	if err != nil {
		return err
	}

	bookInfo := doc.Find("#Cover>img")

	if src, ok := bookInfo.Attr("src"); ok {
		if src != "" {
			t.Books.OriginImageURL = src
		}
	}

	if name, ok := bookInfo.Attr("alt"); ok {
		if name != "" {
			t.Books.Name = name
			return nil
		}
	}

	return errors.New("漫画标题获取失败")
}

// ToFetchChapterList 采集章节 URL 列表
func (t *Mh1234) ToFetchChapterList() (chapterURLList g.SliceStr, err error) {
	doc, err := lfetch.PageSource(t.Books.OriginURL, "utf-8")
	if err != nil {
		return nil, err
	}

	doc.Find("#chapter-list-1 li a").Each(func(i int, aa *goquery.Selection) {
		chapterURL, exist := aa.Attr("href")
		if !exist {
			return
		}

		//chapterURLList = append(chapterURLList, t.WebURL + chapterURL)
		chapterURLList = append(chapterURLList, chapterURL)
	})

	return
}

// ToFetchChapter 获取章节内容
func (t *Mh1234) ToFetchChapter(chapterURL string) (chapterName string, imageURLList g.SliceStr, err error) {
	doc, err := lfetch.PageSource(chapterURL, "utf-8")
	if err != nil {
		return
	}

	scriptImageText := doc.Find("script").Eq(1).Text()

	chapterPath := ""
	pregPath := `chapterPath = "([^"]*)"`
	re1, err := regexp.Compile(pregPath)
	if err != nil {
		return "", nil, err
	}
	path := re1.FindStringSubmatch(scriptImageText)
	if len(path) == 2 {
		chapterPath = path[1]
	}

	imageStr := ""
	pregImageStr := `chapterImages = \[([^\]]*)\]`
	re20, err := regexp.Compile(pregImageStr)
	if err != nil {
		return "", nil, err
	}
	imageStrs := re20.FindStringSubmatch(scriptImageText)
	if len(imageStrs) == 2 {
		imageStr = imageStrs[1]
	}

	if imageStr == "" {
		return "", nil, errors.New("图片列表字符串获取失败")
	}

	pregImages := `"([^"]*)"`
	re21, err := regexp.Compile(pregImages)
	if err != nil {
		return "", nil, err
	}
	imagesList := re21.FindAllStringSubmatch(imageStr, -1)

	if imagesList == nil {
		return
	}

	for _, images := range imagesList {
		if len(images) == 2 {
			imageURLList = append(imageURLList, "/"+strings.TrimLeft(lstrings.Stripslashes(chapterPath+images[1]), "/"))
		}
	}

	if len(imageURLList) == 0 {
		return
	}

	scriptNodes := doc.Find("script")
	scriptCount := scriptNodes.Length()
	scriptNameText := scriptNodes.Eq(scriptCount - 1).Text()

	pregInfo := `SinMH\.initChapter\(([^;]*)\)`
	re3, err := regexp.Compile(pregInfo)
	if err != nil {
		return "", nil, err
	}
	infos := re3.FindStringSubmatch(scriptNameText)

	if len(infos) == 2 {
		infoStr := strings.ReplaceAll(infos[1], `"`, "")
		infoArr := strings.Split(infoStr, ",")

		if len(infoArr) == 4 {
			chapterName = infoArr[1]
		}
	}

	return
}

// NewMh1234 Mh1234 init
func NewMh1234(books *model.TbBooks) *Mh1234 {
	t := &Mh1234{}
	t.Books = books
	t.ResURL = "https://mhpic.dongzaojiage.com"
	return t
}
