package controller

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"github.com/gogf/gf/g"
	"github.com/gogf/gf/g/crypto/gmd5"
	"github.com/gogf/gf/g/os/gfile"
	"github.com/skiy/comic-fetch/app/config"
	"github.com/skiy/comic-fetch/app/library/fetch"
	"github.com/skiy/comic-fetch/app/library/filepath"
	"github.com/skiy/comic-fetch/app/model"
	"github.com/skiy/gf-utils/ucfg"
	"github.com/skiy/gf-utils/udb"
	"github.com/skiy/gf-utils/ulog"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Manhuaniu 漫画牛
type Manhuaniu struct {
	Books  *model.TbBooks
	WebURL string
	ResURL string
}

// NewManhuaniu Manhuaniu init
func NewManhuaniu(books *model.TbBooks) *Manhuaniu {
	t := &Manhuaniu{}
	t.Books = books
	t.ResURL = "https://res.nbhbzl.com/"
	return t
}

// ToFetch 采集
func (t *Manhuaniu) ToFetch() (err error) {
	log := ulog.ReadLog()
	log.Infof("\n正在采集漫画: %s\n源站: %s\n源站漫画URL: %s\n", t.Books.Name, t.Books.OriginWeb, t.Books.OriginURL)

	web := webURL[t.Books.OriginFlag]
	if len(web) < t.Books.OriginWebType {
		return errors.New("runtime error: index out of range for origin_web_type")
	}

	t.WebURL = web[t.Books.OriginWebType]

	// 采集章节列表
	chapterURLList, err := t.ToFetchChapterList()
	if err != nil {
		return err
	}

	if len(chapterURLList) == 0 {
		return errors.New("获取不到章节数据")
	}

	//log.Println(chapterURLList)

	db := udb.GetDatabase()

	// 从数据库中获取已采集的章节列表
	chapters := ([]model.TbChapters)(nil)
	if err = db.Table(config.TbNameChapters).Structs(&chapters); err != nil {
		if err != sql.ErrNoRows {
			return
		}
		err = nil
	}

	// 章节转Map
	chapterStatusMap := map[int]model.TbChapters{}
	for _, chapter := range chapters {
		//log.Println(chapter)
		chapterStatusMap[chapter.OriginID] = chapter
	}

	orderID := len(chapters)
	cfg := ucfg.GetCfg()

	imageLocal := cfg.GetBool("image.local")
	filePath := cfg.GetString("image.path")
	nametype := cfg.GetString("image.nametype")

	// 这里应该用 channel 并发获取章节数据
	for _, chapterURL := range chapterURLList {
		preg := `\/([0-9]*).html`
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
		log.Infof("[URL] %s", fullChapterURL)

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
		preg2 := `第([0-9]*)[话章]`
		re2 := regexp.MustCompile(preg2)
		episodeIDs := re2.FindStringSubmatch(chapterName)

		if len(episodeIDs) > 1 {
			episodeID, _ = strconv.Atoi(strings.Trim(episodeIDs[1], ""))
		}

		log.Infof("[Title] %s, [Image Count] %d", chapterName, len(imageURLList))

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

			if res, err := db.Table(config.TbNameChapters).Data(chapter).Insert(); err != nil {
				log.Warningf("新章节: %s, URL: %s, 保存失败", chapterName, fullChapterURL)
			} else {
				orderID++
				chapterInfo.ID, _ = res.LastInsertId()
			}
		}

		var imageDataArr []model.TbImages
		for index, imageOriginURL := range imageURLList {
			fullImageOriginURL := t.ResURL + imageOriginURL

			log.Debugf("[IMAGE URL] %s", fullImageOriginURL)

			var imageSize int64
			var imageURL string
			isRemote := 1

			// 图片本地化
			if imageLocal {
				if res, err := fetch.GetResponse(fullImageOriginURL, fullChapterURL); err != nil {
					log.Warningf("远程获取图片本地化失败: %v", err)
				} else {
					fileName := fmt.Sprintf("%d-%d-%d", t.Books.ID, chapterInfo.ID, index)
					if strings.EqualFold(nametype, "md5") {
						if name, err := gmd5.Encrypt(fileName); err != nil {
							log.Warningf("图片本地化文件名 MD5 加密失败: %v", err)
						} else {
							fileName = name
						}
					}

					fileExt := filepath.Ext(fullImageOriginURL)
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
				index,
				isRemote,
				timestamp,
			})
		}

		//break
		// 保存图片数据
		if _, err := db.Table(config.TbNameImages).Data(imageDataArr).Batch(len(imageDataArr)).Insert(); err != nil {
			log.Warningf("图片批量保存到数据库失败: %v", err)
		} else { // 图片保存成功
			chapterData := g.Map{
				"status":     0,
				"updated_at": time.Now().Unix(),
			}

			if _, err := db.Table(config.TbNameChapters).Data(chapterData).Where(g.Map{"id": chapterInfo.ID}).Update(); err != nil {
				log.Warningf("章节(%d): %s, URL: %s, 状态(0)更新失败", chapterInfo.ID, chapterName, fullChapterURL)
			}
		}

		//break
	}

	return
}

// ToFetchChapterList 采集章节 URL 列表
func (t *Manhuaniu) ToFetchChapterList() (chapterURLList g.SliceStr, err error) {
	doc, err := fetch.PageSource(t.Books.OriginURL, "utf-8")
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
func (t *Manhuaniu) ToFetchChapter(chapterURL string) (chapterName string, imageURLList g.SliceStr, err error) {
	doc, err := fetch.PageSource(chapterURL, "utf-8")
	if err != nil {
		return
	}

	script2Text := doc.Find("script").Eq(2).Text()

	pregImages := `images\\/[^"]*`
	re, err := regexp.Compile(pregImages)
	if err != nil {
		return "", nil, err
	}
	images := re.FindAllString(script2Text, -1)

	if images == nil {
		return
	}

	for _, image := range images {
		imageURLList = append(imageURLList, strings.ReplaceAll(image, "\\", ""))
	}

	script22Text := doc.Find("script").Eq(22).Text()

	pregInfo := `SinMH\.initChapter\(([^;]*)\)`
	re2, err := regexp.Compile(pregInfo)
	if err != nil {
		return "", nil, err
	}
	infos := re2.FindStringSubmatch(script22Text)

	if len(infos) == 2 {
		infoStr := strings.ReplaceAll(infos[1], `"`, "")
		infoArr := strings.Split(infoStr, ",")

		if len(infoArr) == 4 {
			chapterName = infoArr[1]
		}
	}

	return
}
