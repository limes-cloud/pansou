package yunso

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"

	"pansou/model"
	"pansou/plugin"
	"pansou/util"
	jsonutil "pansou/util/json"
)

const (
	yunsoSearchAPI       = "https://www.yunso.net/api/Core/search2"
	yunsoSearchPage      = "https://www.yunso.net/index/user/s"
	yunsoDecryptKey      = "pWz1vnL1fTkOvTMW3f9M1jJWfneUIh50"
	yunsoDefaultScope    = "0"
	yunsoDefaultPageSize = 15
	yunsoDefaultMaxPages = 1
	yunsoDefaultTimeout  = 30 * time.Second
)

var (
	yunsoDatetimeRegex = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}`)
	yunsoTypeCodeRegex = regexp.MustCompile(`/assets/xyso/(\d+)\.png`)
	yunsoDecryptBytes  = []byte(yunsoDecryptKey)
	// yunsoDefaultModes 每次搜索并发请求的模式（2026-09-15 实测：按关键词字数分流不可靠，
	// 两种模式返回结果分布不同，固定双模式并发以覆盖更多结果）
	yunsoDefaultModes = []string{"90001", "90002"}
	// yunsoDefaultStypes 每次搜索并发请求的类型：1=综合，20500=夸克（对应 mapDiskType 的 TypeCode）
	yunsoDefaultStypes = []string{"1", "20500"}

	// yunsoHTTPClient 专用 HTTP 客户端：禁用 HTTP/2。
	// 实测（2026-09-15）：yunso 前端的 Cloudflare 对数据中心 IP（如腾讯云 106）的
	// HTTP/2 请求做指纹校验，Go 的 h2 指纹与浏览器不同会被判别为程序并强制人机验证
	// （code:-2 需要完成人机验证，响应 89B）；HTTP/1.1 请求正常放行。
	// 家宽 IP 下 h2 也能通过，为统一行为一律走 HTTP/1.1。
	yunsoHTTPClient  *http.Client
	yunsoHTTPOnce    sync.Once
)

// getYunsoHTTPClient 返回禁用 HTTP/2 的单例客户端（30s 超时）
func getYunsoHTTPClient() *http.Client {
	yunsoHTTPOnce.Do(func() {
		yunsoHTTPClient = &http.Client{
			Timeout: yunsoDefaultTimeout,
			Transport: &http.Transport{
				ForceAttemptHTTP2: false,
				// 禁用 HTTP/2 协商，仅保留 HTTP/1.1
				TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
			},
		}
	})
	return yunsoHTTPClient
}

func init() {
	plugin.RegisterGlobalPlugin(NewYunsoAsyncPlugin())
}

// YunsoAsyncPlugin 小云搜索异步插件
type YunsoAsyncPlugin struct {
	*plugin.BaseAsyncPlugin
}

// YunsoAPIResponse 小云搜索接口响应
type YunsoAPIResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data string `json:"data"`
}

// YunsoItem 小云搜索结果项
type YunsoItem struct {
	FullID       string
	QID          string
	Title        string
	EncryptedURL string
	URL          string
	Password     string
	TypeCode     string
	TypeName     string
	Preview      string
	FileSummary  string
	Datetime     time.Time
	Badges       []string
}

// NewYunsoAsyncPlugin 创建新的小云搜索插件
func NewYunsoAsyncPlugin() *YunsoAsyncPlugin {
	// skipServiceFilter=true：站点结果标题多为「…黄蓉…」片段，不含完整关键词（如「黄蓉杨贵妃」），
	// Service 层 mergeResultsByType 的标题包含过滤会将其全部丢弃，故跳过该层过滤
	return &YunsoAsyncPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPluginWithFilter("yunso", 3, true),
	}
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *YunsoAsyncPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含 IsFinal 标记的结果
func (p *YunsoAsyncPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.doSearch, p.MainCacheKey, ext)
}

// doSearch 实际搜索实现：并发请求各 mode × stype（每个组合最多 yunsoDefaultMaxPages 页），合并结果。
// 忽略框架传入的 client，统一使用禁 HTTP/2 的 yunso HTTP 客户端（见 getYunsoHTTPClient）。
func (p *YunsoAsyncPlugin) doSearch(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	requestCnt := len(yunsoDefaultModes) * len(yunsoDefaultStypes) * yunsoDefaultMaxPages
	resultChan := make(chan []YunsoItem, requestCnt)
	errChan := make(chan error, requestCnt)

	httpClient := getYunsoHTTPClient()
	var wg sync.WaitGroup
	for _, mode := range yunsoDefaultModes {
		for _, stype := range yunsoDefaultStypes {
			for page := 1; page <= yunsoDefaultMaxPages; page++ {
				wg.Add(1)
				go func(m, st string, pageNum int) {
					defer wg.Done()

					items, err := p.searchPage(httpClient, keyword, m, st, pageNum)
					if err != nil {
						errChan <- fmt.Errorf("mode %s stype %s page %d search failed: %w", m, st, pageNum, err)
						return
					}
					resultChan <- items
				}(mode, stype, page)
			}
		}
	}

	go func() {
		wg.Wait()
		close(resultChan)
		close(errChan)
	}()

	var allItems []YunsoItem
	for items := range resultChan {
		allItems = append(allItems, items...)
	}

	var errs []error
	for err := range errChan {
		errs = append(errs, err)
	}

	if len(allItems) == 0 && len(errs) > 0 {
		return nil, errs[0]
	}

	uniqueItems := p.deduplicateItems(allItems)
	// 不经过 FilterResultsByKeyword：该过滤器要求标题/内容包含完整关键词子串，
	// 对「黄蓉杨贵妃」这类无空格中文长词会全部误滤（站内标题多为"…黄蓉…"片段），
	// yunso 结果本身已按相关性排序，直接返回
	return p.convertResults(uniqueItems), nil
}

// searchPage 按 mode、stype 与页码请求一页搜索结果
func (p *YunsoAsyncPlugin) searchPage(client *http.Client, keyword, mode, stype string, page int) ([]YunsoItem, error) {
	ctx, cancel := context.WithTimeout(context.Background(), yunsoDefaultTimeout)
	defer cancel()

	params := url.Values{}
	params.Set("requestID", "")
	params.Set("mode", mode)
	params.Set("scope_content", yunsoDefaultScope)
	params.Set("stype", stype)
	params.Set("wd", keyword)
	params.Set("uk", "")
	params.Set("page", strconv.Itoa(page))
	params.Set("limit", strconv.Itoa(yunsoDefaultPageSize))
	params.Set("screen_filetype", "")
	params.Set("screen_time", "")
	params.Set("screen_size", "")
	params.Set("screen_sortby", "")

	// 与浏览器抓包对齐（2026-09-14 实测）：云服务器 IP 下缺 body 或完整浏览器头会被
	// yunso 当作异常请求只回「聚合展示」摘要页（无 search-item），务必整组对齐。
	// xyso_turnstile_token 为空串即可：业务参数在 query，body 只带该字段标识客户端请求。
	searchURL := yunsoSearchAPI + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, searchURL, strings.NewReader("xyso_turnstile_token="))
	if err != nil {
		return nil, fmt.Errorf("create request failed: %w", err)
	}

	referer := yunsoSearchPage + "?wd=" + url.QueryEscape(keyword)
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("Origin", "https://www.yunso.net")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Referer", referer)
	req.Header.Set("Sec-Ch-Ua", `"Google Chrome";v="153", "Not_A Brand";v="8", "Chromium";v="153"`)
	req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	req.Header.Set("Sec-Ch-Ua-Platform", `"macOS"`)
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response failed: %w", err)
	}

	var apiResp YunsoAPIResponse
	if err := jsonutil.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("decode response failed: %w", err)
	}

	if apiResp.Code != 0 {
		return nil, fmt.Errorf("api returned error: %s", apiResp.Msg)
	}

	return p.parseItems(apiResp.Data)
}

func (p *YunsoAsyncPlugin) parseItems(fragment string) ([]YunsoItem, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(`<div id="yunso-root">` + fragment + `</div>`))
	if err != nil {
		return nil, fmt.Errorf("parse html failed: %w", err)
	}

	items := make([]YunsoItem, 0, 16)
	doc.Find("div.layui-card[data-qid]").Each(func(_ int, card *goquery.Selection) {
		anchor := card.Find(`a[onclick*="open_sid"]`).First()
		if anchor.Length() == 0 {
			return
		}

		title := cleanYunsoText(anchor.Text())
		if title == "" {
			return
		}

		encryptedURL, _ := anchor.Attr("url")
		decryptedURL := ""
		if encryptedURL != "" {
			if decoded, err := decryptYunsoURL(encryptedURL); err == nil {
				decryptedURL = decoded
			}
		}

		fileSummary := cleanYunsoText(card.Find(".layui-card-body span").First().Text())
		if strings.Contains(strings.ToUpper(fileSummary), "N/A") {
			fileSummary = ""
		}

		item := YunsoItem{
			Title:        title,
			EncryptedURL: strings.TrimSpace(encryptedURL),
			URL:          strings.TrimSpace(decryptedURL),
			Preview:      cleanYunsoText(card.Find("p.result.container.p").First().Text()),
			FileSummary:  fileSummary,
			Datetime:     parseYunsoDatetime(card.Find(".layui-card-header").Text()),
			TypeName:     cleanYunsoText(card.Find(`img[src*="/assets/xyso/"]`).First().AttrOr("alt", "")),
			TypeCode:     extractYunsoTypeCode(card.Find(`img[src*="/assets/xyso/"]`).First().AttrOr("src", "")),
			Password:     cleanYunsoText(anchor.AttrOr("pa", "")),
			FullID:       strings.TrimSpace(anchor.AttrOr("id", "")),
			QID:          strings.TrimSpace(card.AttrOr("data-qid", "")),
			Badges:       extractYunsoBadges(card),
		}

		if item.Password == "" {
			item.Password = extractYunsoPassword(item.URL)
		}

		items = append(items, item)
	})

	return items, nil
}

func (p *YunsoAsyncPlugin) deduplicateItems(items []YunsoItem) []YunsoItem {
	uniqueMap := make(map[string]YunsoItem)

	for _, item := range items {
		key := item.URL
		if key == "" {
			key = item.FullID
		}
		if key == "" {
			key = item.QID + "|" + item.Title
		}

		existing, exists := uniqueMap[key]
		if !exists || scoreYunsoItem(item) > scoreYunsoItem(existing) {
			uniqueMap[key] = item
		}
	}

	result := make([]YunsoItem, 0, len(uniqueMap))
	for _, item := range uniqueMap {
		result = append(result, item)
	}
	return result
}

func scoreYunsoItem(item YunsoItem) int {
	score := 0
	if item.URL != "" {
		score += 8
	}
	if item.Password != "" {
		score += 5
	}
	if !item.Datetime.IsZero() {
		score += 3
	}
	if item.FileSummary != "" {
		score += 2
	}
	if item.Preview != "" {
		score += 2
	}
	if item.TypeCode != "" {
		score++
	}
	return score
}

func (p *YunsoAsyncPlugin) convertResults(items []YunsoItem) []model.SearchResult {
	results := make([]model.SearchResult, 0, len(items))

	for i, item := range items {
		if strings.TrimSpace(item.URL) == "" {
			continue
		}

		contentParts := make([]string, 0, 3)
		if item.Preview != "" {
			contentParts = append(contentParts, item.Preview)
		}
		if item.FileSummary != "" {
			contentParts = append(contentParts, item.FileSummary)
		}
		if item.TypeName != "" {
			contentParts = append(contentParts, "网盘: "+item.TypeName)
		}

		tags := uniqueYunsoStrings(append([]string{item.TypeName}, item.Badges...))
		if len(tags) == 0 {
			tags = nil
		}

		uniqueID := fmt.Sprintf("yunso-%s", item.FullID)
		if item.FullID == "" {
			uniqueID = fmt.Sprintf("yunso-%s-%d", item.QID, i)
		}

		results = append(results, model.SearchResult{
			UniqueID: uniqueID,
			Channel:  "",
			Datetime: item.Datetime,
			Title:    item.Title,
			Content:  strings.Join(contentParts, "\n"),
			Tags:     tags,
			Links: []model.Link{
				{
					URL:      item.URL,
					Type:     p.mapDiskType(item.TypeCode, item.URL),
					Password: item.Password,
					Datetime: item.Datetime,
				},
			},
		})
	}

	return results
}

func (p *YunsoAsyncPlugin) mapDiskType(typeCode string, rawURL string) string {
	switch strings.TrimSpace(typeCode) {
	case "1":
		return "baidu"
	case "20100":
		return "aliyun"
	case "20500":
		return "quark"
	case "20000":
		return "tianyi"
	case "20300":
		return "mobile"
	case "20400":
		return "xunlei"
	case "20501":
		return "uc"
	case "20600":
		return "lanzou"
	}

	lowerURL := strings.ToLower(strings.TrimSpace(rawURL))
	if strings.Contains(lowerURL, "fast.uc.cn") || strings.Contains(lowerURL, "uc.cn") {
		return "uc"
	}
	return util.GetLinkType(lowerURL)
}

func decryptYunsoURL(value string) (string, error) {
	value = strings.TrimSpace(html.UnescapeString(value))
	if value == "" {
		return "", fmt.Errorf("empty encrypted url")
	}
	// Newer responses expose the share URL directly. Keep the legacy
	// base64/XOR path below for older results.
	if strings.HasPrefix(strings.ToLower(value), "http://") || strings.HasPrefix(strings.ToLower(value), "https://") {
		return value, nil
	}

	decoded, err := decodeYunsoBase64(value)
	if err != nil {
		return "", err
	}

	rawText := strings.TrimSpace(string(decoded))
	if strings.HasPrefix(rawText, "http://") || strings.HasPrefix(rawText, "https://") {
		return rawText, nil
	}

	result := make([]byte, len(decoded))
	for i := range decoded {
		result[i] = decoded[i] ^ yunsoDecryptBytes[i%len(yunsoDecryptBytes)]
	}

	return strings.TrimSpace(string(result)), nil
}

func decodeYunsoBase64(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("empty encrypted url")
	}

	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil {
		return decoded, nil
	}

	if mod := len(value) % 4; mod != 0 {
		value += strings.Repeat("=", 4-mod)
	}
	return base64.StdEncoding.DecodeString(value)
}

func extractYunsoTypeCode(iconSrc string) string {
	matches := yunsoTypeCodeRegex.FindStringSubmatch(iconSrc)
	if len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

func extractYunsoBadges(card *goquery.Selection) []string {
	badges := make([]string, 0, 2)
	card.Find(".badge").Each(func(_ int, badge *goquery.Selection) {
		text := cleanYunsoText(badge.Text())
		if text != "" {
			badges = append(badges, text)
		}
	})
	return uniqueYunsoStrings(badges)
}

func extractYunsoPassword(rawURL string) string {
	if rawURL == "" {
		return ""
	}

	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}

	for _, key := range []string{"pwd", "pass", "password"} {
		if value := strings.TrimSpace(parsedURL.Query().Get(key)); value != "" {
			return value
		}
	}
	return ""
}

func parseYunsoDatetime(text string) time.Time {
	match := yunsoDatetimeRegex.FindString(text)
	if match == "" {
		return time.Time{}
	}

	parsedTime, err := time.Parse("2006-01-02 15:04:05", match)
	if err != nil {
		return time.Time{}
	}
	return parsedTime
}

func cleanYunsoText(value string) string {
	value = html.UnescapeString(value)
	value = strings.Join(strings.Fields(value), " ")
	return strings.TrimSpace(value)
}

func uniqueYunsoStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = cleanYunsoText(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
