package api

import (
	"fmt"
	"html"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"pansou/config"
)

// seoHotKeywords sitemap 收录的落地页搜索词：与前端（link-nest/static/home.js CATEGORIES）
// 展示的可搜索分类 tag 对齐，点击分类即按该词搜索
var seoHotKeywords = []string{
	"电视剧", "电影", "国产剧", "美剧", "韩剧", "日剧", "泰剧", "港剧",
	"综艺", "纪录片", "动漫", "国漫", "日漫", "漫剧", "动画电影", "情景剧",
	"小说", "电子书", "漫画", "有声小说", "言情小说", "玄幻小说", "悬疑小说", "科幻小说",
	"写真", "素材", "图片素材", "视频素材", "音效", "字体", "图标", "PPT", "模板",
	"简历模板", "工作报告", "办公文档", "合同模板", "软件", "电脑软件", "手机软件", "游戏",
	"单机游戏", "手游", "Steam游戏", "音乐", "无损音乐", "车载音乐", "教程", "课程",
	"网课", "考研", "考公", "英语", "编程", "设计", "摄影", "PS教程", "Excel教程",
	"壁纸", "表情包", "学习资料", "儿童早教", "纪录片合集", "经典老歌", "现场Live", "MV合集",
}

// RobotsTxtHandler 返回 robots.txt：放行全部爬虫
func RobotsTxtHandler(c *gin.Context) {
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.String(200, "User-agent: *\nAllow: /\n")
}

// SitemapHandler 返回 sitemap.xml：首页 + 热门词落地页（URL 按请求 Host 动态生成）
func SitemapHandler(c *gin.Context) {
	base := seoBaseURL(c)

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
	b.WriteString("  <url><loc>" + base + "/</loc><changefreq>daily</changefreq><priority>1.0</priority></url>\n")
	for _, kw := range seoHotKeywords {
		loc := base + "/?kw=" + url.QueryEscape(kw)
		b.WriteString("  <url><loc>" + loc + "</loc><changefreq>daily</changefreq><priority>0.8</priority></url>\n")
	}
	b.WriteString("</urlset>")

	c.Header("Content-Type", "application/xml; charset=utf-8")
	c.String(200, b.String())
}

// IndexHandler 落地页：带完整 SEO meta（title 唯一化、description、keywords、og 标签）。
// 无 ?kw= 时为首页；带 ?kw= 时生成热门词搜索结果页模板。
func IndexHandler(c *gin.Context) {
	kw := strings.TrimSpace(c.Query("kw"))
	base := seoBaseURL(c)

	// 依据是否带关键词生成唯一化标题与描述
	var (
		title       string
		description string
		keywords    string
		canonical   string
	)
	if kw != "" {
		safeKw := html.EscapeString(kw)
		title = fmt.Sprintf("%s 网盘资源搜索 - PanSou", safeKw)
		description = fmt.Sprintf("搜索「%s」网盘资源，PanSou 聚合百度网盘、夸克网盘、阿里云盘等多个平台结果，按类型分类、按时间与相关度智能排序，结果实时更新。", safeKw)
		keywords = fmt.Sprintf("%s,网盘搜索,%s百度网盘,%s夸克网盘,资源下载", safeKw, safeKw, safeKw)
		canonical = base + "/?kw=" + url.QueryEscape(kw)
	} else {
		title = "PanSou 网盘资源搜索 - 百度/夸克/阿里云盘一站式搜索"
		description = "PanSou 高性联网盘资源搜索引擎，聚合百度网盘、夸克网盘、阿里云盘、天翼云盘、115网盘等14种网盘类型，支持关键词/网盘类型/来源筛选，并发搜索智能排序，Docker一键部署。"
		keywords = "网盘搜索,百度网盘搜索,夸克网盘搜索,阿里云盘搜索,网盘资源,网盘API,pansou"
		canonical = base + "/"
	}

	// 站点动态信息（插件/频道数）
	pluginCount, channelCount := seoSiteStats()

	htmlContent := fmt.Sprintf(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
	<meta charset="UTF-8">
	<meta name="viewport" content="width=device-width, initial-scale=1.0">
	<title>%s</title>
	<meta name="description" content="%s">
	<meta name="keywords" content="%s">
	<link rel="canonical" href="%s">
	<meta property="og:site_name" content="PanSou 网盘搜索">
	<meta property="og:type" content="website">
	<meta property="og:title" content="%s">
	<meta property="og:description" content="%s">
	<meta property="og:url" content="%s">
	<meta name="robots" content="index,follow">
	<style>
		body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",PingFang SC,"Microsoft YaHei",sans-serif;max-width:720px;margin:0 auto;padding:48px 24px;color:#222;line-height:1.8}
		h1{font-size:28px;margin-bottom:8px}
		.meta{color:#888;font-size:14px;margin-bottom:24px}
		ul{color:#555}
		code{background:#f4f4f4;padding:2px 6px;border-radius:4px;font-size:14px}
		footer{margin-top:48px;color:#aaa;font-size:12px;border-top:1px solid #eee;padding-top:16px}
	</style>
</head>
<body>
	<h1>PanSou 网盘资源搜索</h1>
	<p class="meta">高性联网盘资源搜索引擎 · %d个插件 · %d个频道 · 14种网盘类型</p>
	<p>输入关键词即可搜索全网盘资源：</p>
	<ul>
		<li>支持网盘：百度、夸克、阿里云、天翼、115、PikPak、迅雷、123、UC、移动云盘等</li>
		<li>按网盘类型分类浏览，结果按时间与相关度智能排序</li>
		<li>提供公开 API（<code>GET /api/search?kw=关键词&amp;res=merged_by_type</code>），支持 Docker 一键部署</li>
	</ul>
	<footer>© %d PanSou · 网盘资源搜索 API 服务</footer>
</body>
</html>`, title, description, keywords, canonical, title, description, canonical,
		pluginCount, channelCount, time.Now().Year())

	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(200, htmlContent)
}

// seoBaseURL 拼接站点基础 URL：优先使用配置的 SITE_BASE_URL，否则按请求 Host 动态生成
func seoBaseURL(c *gin.Context) string {
	if config.AppConfig != nil && config.AppConfig.SiteBaseURL != "" {
		return config.AppConfig.SiteBaseURL
	}
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s", scheme, c.Request.Host)
}

// seoSiteStats 获取插件与频道数量（用于落地页展示）
func seoSiteStats() (pluginCount, channelCount int) {
	if searchService != nil && searchService.GetPluginManager() != nil {
		pluginCount = len(searchService.GetPluginManager().GetPlugins())
	}
	if config.AppConfig != nil {
		channelCount = len(config.AppConfig.DefaultChannels)
	}
	return
}