/*
EmbyX - 直播中转服务
© 2026 谢週五 (https://juneix.github.io)
*/
package main

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 配置文件 live.json 对应的结构体
type ManualChannel struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Logo string `json:"logo,omitempty"`
}

type AutoChannel struct {
	Name     string `json:"name"`
	Platform string `json:"platform"`
	RoomID   string `json:"room_id"`
	Logo     string `json:"logo,omitempty"`
}

type LiveConfig struct {
	ManualList []ManualChannel `json:"manual_list"`
	AutoList   []AutoChannel   `json:"auto_list"`
}

// 包含 BaseURL 的保存请求结构体
type SaveRequest struct {
	LiveConfig
	BaseURL string `json:"base_url"`
}

var (
	configPath = "live/live.json"
	strmOutDir = "./strm_out"
	mu         sync.Mutex
)

func main() {
	// 读取配置的环境变量
	if envPath := os.Getenv("CONFIG_PATH"); envPath != "" {
		configPath = envPath
	}
	if envOutDir := os.Getenv("STRM_OUT_DIR"); envOutDir != "" {
		strmOutDir = envOutDir
	}

	// 确保 strm 输出目录存在
	if err := os.MkdirAll(strmOutDir, 0755); err != nil {
		log.Fatalf("无法创建 strm 输出目录: %v", err)
	}

	// 注册路由
	http.HandleFunc("/api/config", handleConfig)
	http.HandleFunc("/api/fetch_avatar", handleFetchAvatar)
	http.HandleFunc("/api/scan_library", handleScanLibrary)
	http.HandleFunc("/api/logo", handleGetLogo)
	http.HandleFunc("/play", handlePlayRedirect)

	// 后台运行在 8191 端口 (支持 LIVE_PORT 或 PROXY_PORT 环境变量控制)
	port := "8191"
	if envPort := os.Getenv("LIVE_PORT"); envPort != "" {
		port = envPort
	} else if envPort := os.Getenv("PROXY_PORT"); envPort != "" {
		port = envPort
	}

	log.Printf("📱 EmbyX 直播中转已启动，监听端口: %s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("服务器启动失败: %v", err)
	}
}

// ==================== 1. API 路由处理器 ====================

// 读写配置文件，并自动同步 `.strm` 文件夹
func handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.Header().Set("Access-Control-Allow-Methods", "*")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	mu.Lock()
	defer mu.Unlock()

	// 智能判定是否为本地测试环境 (Referer 或 Origin 包含 5500 端口)
	path := configPath
	outDir := strmOutDir
	referer := r.Header.Get("Referer")
	origin := r.Header.Get("Origin")
	if strings.Contains(referer, ":5500") || strings.Contains(origin, ":5500") {
		path = "strm_test/live.json"
		outDir = "./strm_test"
	}

	if r.Method == http.MethodGet {
		// GET 请求: 返回当前的配置
		data, err := os.ReadFile(path)
		if err != nil {
			// 文件不存在则返回空模板
			w.Write([]byte(`{"manual_list":[],"auto_list":[]}`))
			return
		}
		w.Write(data)
		return
	}

	if r.Method == http.MethodPost {
		// POST 请求: 保存配置并同步 strm
		var req SaveRequest
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&req); err != nil {
			http.Error(w, `{"message":"无效的JSON数据"}`, http.StatusBadRequest)
			return
		}

		// 格式化回写 live.json
		jsonData, err := json.MarshalIndent(req.LiveConfig, "", "  ")
		if err != nil {
			http.Error(w, `{"message":"JSON序列化失败"}`, http.StatusInternalServerError)
			return
		}

		if err := os.WriteFile(path, jsonData, 0644); err != nil {
			http.Error(w, `{"message":"写入配置文件失败"}`, http.StatusInternalServerError)
			return
		}

		// 同步指定目录的 strm 与海报图片
		go syncStrmDirectory(req.LiveConfig, req.BaseURL, outDir)

		w.Write([]byte(`{"status":"success"}`))
		return
	}

	w.WriteHeader(http.StatusMethodNotAllowed)
}

// 自动联想拉取主播头像
func handleFetchAvatar(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	platform := r.URL.Query().Get("platform")
	roomID := r.URL.Query().Get("room_id")

	if platform == "" || roomID == "" {
		http.Error(w, `{"message":"缺少平台或房间号"}`, http.StatusBadRequest)
		return
	}

	var avatar, nickname string
	var err error

	switch platform {
	case "douyin":
		avatar, nickname, err = fetchDouyinInfo(roomID)
	case "kuaishou":
		avatar, nickname, err = fetchKuaishouInfo(roomID)
	case "douyu":
		avatar, nickname, err = fetchDouyuInfo(roomID)
	case "huya":
		avatar, nickname, err = fetchHuyaInfo(roomID)
	case "bilibili":
		avatar, nickname, err = fetchBilibiliInfo(roomID)
	default:
		err = fmt.Errorf("不支持的平台")
	}

	if err != nil {
		log.Printf("获取主播信息失败 [%s:%s]: %v", platform, roomID, err)
		http.Error(w, fmt.Sprintf(`{"message":"%v"}`, err), http.StatusInternalServerError)
		return
	}

	resp := map[string]string{
		"avatar":   avatar,
		"nickname": nickname,
	}
	json.NewEncoder(w).Encode(resp)
}

// 发起 Emby 后台库扫描
func handleScanLibrary(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// 直接从请求参数或默认配置里通知 Emby (如果有配置的话)
	// 在 live.html 中也可以让前端直接通过 ajax 请求 Emby。我们这里预留 Go 发送
	// 由于前端活泼的配置，我们可以读取 live.json 里可能的设置，或者直接支持前端在 POST 里指定 Server 和 Token
	type EmbyNotify struct {
		Server string `json:"server"`
		Token  string `json:"token"`
	}

	var notify EmbyNotify
	json.NewDecoder(r.Body).Decode(&notify)

	if notify.Server == "" || notify.Token == "" {
		// 如果前端没给，尝试从环境变量获取默认配置
		notify.Server = os.Getenv("EMBY_SERVER")
		notify.Token = os.Getenv("EMBY_TOKEN")
	}

	if notify.Server == "" || notify.Token == "" {
		// 如果都没有，返回提示让前端自行发送或者直接算成功 (因为 live.html 也会在前台自己发送以做双保险)
		w.Write([]byte(`{"status":"skipped","message":"未提供 Emby Server 和 Token 变量"}`))
		return
	}

	// 格式化 Emby 刷新 URL
	u := fmt.Sprintf("%s/emby/Library/Media/Refresh?api_key=%s", strings.TrimSuffix(notify.Server, "/"), notify.Token)
	resp, err := http.Post(u, "application/json", nil)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"连接Emby失败: %v"}`, err), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	w.Write([]byte(`{"status":"success"}`))
}

// 播放时实时 302 重定向或中继到真实的播放地址
func handlePlayRedirect(w http.ResponseWriter, r *http.Request) {
	// 强行注入局域网 CORS 放行头以支持 mpegts.js / Hls.js 跨域请求
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.Header().Set("Access-Control-Allow-Methods", "*")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	name := r.URL.Query().Get("id")
	playType := r.URL.Query().Get("type")

	if name == "" {
		http.Error(w, "缺少 id 参数", http.StatusBadRequest)
		return
	}

	var playURL string
	var err error

	// 智能多路径降级查找：优先尝试读取本地测试 strm_test/live.json
	data, errLocal := os.ReadFile("strm_test/live.json")
	if errLocal == nil {
		var config LiveConfig
		if json.Unmarshal(data, &config) == nil {
			playURL, err = findPlayURLInConfig(config, name, playType)
		}
	}

	// 如果在测试配置中没找到或出错，则回退到默认的根目录 live.json 配置
	if playURL == "" {
		dataDefault, errDefault := os.ReadFile(configPath)
		if errDefault != nil {
			http.Error(w, "未发现任何直播源配置文件", http.StatusNotFound)
			return
		}
		var configDefault LiveConfig
		if err = json.Unmarshal(dataDefault, &configDefault); err != nil {
			http.Error(w, "配置文件损坏", http.StatusInternalServerError)
			return
		}
		playURL, err = findPlayURLInConfig(configDefault, name, playType)
	}

	if err != nil {
		http.Error(w, fmt.Sprintf("实时解析流失败: %v", err), http.StatusInternalServerError)
		return
	}

	if playURL == "" {
		http.Error(w, "主播当前可能下播或频道未找到", http.StatusNotFound)
		return
	}

	// HLS 直接交给 Emby；只有 FLV 需要经过本服务中继。
	lowerPlayURL := strings.ToLower(playURL)
	isFlv := strings.Contains(lowerPlayURL, ".flv") || strings.Contains(lowerPlayURL, "flv=") || strings.Contains(lowerPlayURL, "/flv")

	if !isFlv {
		log.Printf("📺 HLS/直连协议流走 302 重定向: %s", name)
		http.Redirect(w, r, playURL, http.StatusFound)
		return
	}

	// Chrome/Edge FLV 流：高性能 io.Copy 二进制中继 (Timeout=0 永不超时防止断流)
	client := &http.Client{Timeout: 0}
	req, err := http.NewRequest("GET", playURL, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf("创建中继请求失败: %v", err), http.StatusInternalServerError)
		return
	}

	// 智能伪造请求头以破除平台防盗链
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	if strings.Contains(playURL, "bilibili") || strings.Contains(playURL, "bilivideo") {
		req.Header.Set("Referer", "https://live.bilibili.com/")
	} else if strings.Contains(playURL, "huya") {
		req.Header.Set("Referer", "https://m.huya.com/")
	} else if strings.Contains(playURL, "douyu") {
		req.Header.Set("Referer", "https://www.douyu.com/")
	} else if strings.Contains(playURL, "kuaishou") || strings.Contains(playURL, "kwaicdn") || strings.Contains(playURL, "yximgs") {
		req.Header.Set("Referer", "https://live.kuaishou.com/")
	}

	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("连接直播源流失败: %v", err), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		http.Error(w, fmt.Sprintf("直播源返回 HTTP %d", resp.StatusCode), http.StatusBadGateway)
		return
	}

	// chunked 块分发，保持原画无损低延时
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("Transfer-Encoding", "chunked")

	log.Printf("💻 FLV 中继启动: %s", name)
	_, _ = io.Copy(w, resp.Body)
}

// 辅助函数：在一套 LiveConfig 配置中检索出指定主播/频道的播放 URL (包含实时解析)
func findPlayURLInConfig(config LiveConfig, name string, playType string) (string, error) {
	if playType == "manual" {
		for _, item := range config.ManualList {
			if item.Name == name {
				return item.URL, nil
			}
		}
	} else {
		for _, item := range config.AutoList {
			if item.Name == name {
				url, err := parseLiveStream(item.Platform, item.RoomID)
				if err != nil {
					return "", err
				}
				return url, nil
			}
		}
	}
	return "", nil
}

// ==================== 2. 直播流解析核心算法 ====================

const (
	desktopUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	mobileUserAgent  = "ios/7.830 (ios 17.0; iPhone 15)"
)

func newLiveClient() *http.Client {
	return &http.Client{Timeout: 8 * time.Second}
}

func readLiveResponse(resp *http.Response, platform string) ([]byte, error) {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s 接口返回 HTTP %d", platform, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("%s 接口返回空数据", platform)
	}
	return body, nil
}

func normalizeRoomID(roomID string) string {
	roomID = strings.TrimSpace(roomID)
	if parsed, err := url.Parse(roomID); err == nil && parsed.Host != "" {
		if id := strings.Trim(strings.TrimSpace(parsed.Path), "/"); id != "" {
			parts := strings.Split(id, "/")
			return parts[len(parts)-1]
		}
	}
	return strings.Trim(roomID, "/")
}

func md5Hex(value string) string {
	sum := md5.Sum([]byte(value))
	return fmt.Sprintf("%x", sum)
}

type douyinUser struct {
	Nickname    string `json:"nickname"`
	AvatarThumb struct {
		URLList []string `json:"url_list"`
	} `json:"avatar_thumb"`
}

type douyinRoom struct {
	IDStr  string     `json:"id_str"`
	Title  string     `json:"title"`
	Status int        `json:"status"`
	Owner  douyinUser `json:"owner"`
	Cover  struct {
		URLList []string `json:"url_list"`
	} `json:"cover"`
	StreamURL struct {
		HLS               map[string]string `json:"hls_pull_url_map"`
		DefaultResolution string            `json:"default_resolution"`
	} `json:"stream_url"`
}

type douyinRoomData struct {
	Room   douyinRoom
	Anchor douyinUser
	WebRID string
}

func extractPaceJSON(input string) []string {
	const marker = "__pace_f"
	const endTag = "</script>"
	var decodedParts []string

	for {
		markerIndex := strings.Index(input, marker)
		if markerIndex < 0 {
			break
		}
		input = input[markerIndex+len(marker):]
		quoteIndex := strings.Index(input, `"`)
		if quoteIndex < 0 {
			continue
		}
		input = input[quoteIndex+1:]
		endIndex := strings.Index(input, endTag)
		if endIndex < 0 {
			continue
		}
		lastQuote := strings.LastIndex(input[:endIndex], `"`)
		if lastQuote < 0 {
			continue
		}
		var decoded string
		if json.Unmarshal([]byte(`"`+input[:lastQuote]+`"`), &decoded) == nil {
			decodedParts = append(decodedParts, decoded)
		}
	}

	var payloads []string
	for _, line := range strings.Split(strings.Join(decodedParts, "\n"), "\n") {
		start := strings.IndexAny(line, "[{")
		end := strings.LastIndexAny(line, "]}")
		if start >= 0 && end >= start {
			payloads = append(payloads, line[start:end+1])
		}
	}
	return payloads
}

func parseDouyinPage(body []byte) (douyinRoomData, error) {
	type pageState struct {
		State struct {
			RoomStore struct {
				RoomInfo struct {
					Room   douyinRoom `json:"room"`
					Anchor douyinUser `json:"anchor"`
					WebRID string     `json:"web_rid"`
				} `json:"roomInfo"`
			} `json:"roomStore"`
		} `json:"state"`
	}

	for _, payload := range extractPaceJSON(string(body)) {
		var values []json.RawMessage
		if json.Unmarshal([]byte(payload), &values) != nil {
			continue
		}
		for _, raw := range values {
			var page pageState
			if json.Unmarshal(raw, &page) != nil {
				continue
			}
			info := page.State.RoomStore.RoomInfo
			if info.Room.IDStr != "" || info.Room.Owner.Nickname != "" || info.Anchor.Nickname != "" {
				return douyinRoomData{Room: info.Room, Anchor: info.Anchor, WebRID: info.WebRID}, nil
			}
		}
	}
	return douyinRoomData{}, fmt.Errorf("未能从抖音页面读取直播间数据，可能触发了风控")
}

func fetchDouyinRoomOnce(roomID, cookie string) (douyinRoomData, error) {
	roomID = normalizeRoomID(roomID)
	req, err := http.NewRequest(http.MethodGet, "https://live.douyin.com/"+url.PathEscape(roomID), nil)
	if err != nil {
		return douyinRoomData{}, err
	}
	req.Header.Set("User-Agent", desktopUserAgent)
	req.Header.Set("Referer", "https://live.douyin.com/")
	if cookie == "" {
		cookie = "__ac_nonce=064caded4009deafd8b89"
	}
	req.Header.Set("Cookie", cookie)
	resp, err := newLiveClient().Do(req)
	if err != nil {
		return douyinRoomData{}, err
	}
	body, err := readLiveResponse(resp, "抖音")
	if err != nil {
		return douyinRoomData{}, err
	}
	return parseDouyinPage(body)
}

func fetchDouyinRoom(roomID string) (douyinRoomData, error) {
	data, anonymousErr := fetchDouyinRoomOnce(roomID, "")
	if anonymousErr == nil {
		return data, nil
	}
	if cookie := strings.TrimSpace(os.Getenv("LIVE_DOUYIN_COOKIE")); cookie != "" {
		if data, err := fetchDouyinRoomOnce(roomID, cookie); err == nil {
			return data, nil
		}
	}
	return douyinRoomData{}, anonymousErr
}

func selectDouyinHLS(streams map[string]string, defaultResolution string) string {
	if defaultResolution != "" && streams[defaultResolution] != "" {
		return streams[defaultResolution]
	}
	priorities := []string{"ORIGIN", "FULL_HD1", "FULL_HD", "HD1", "HD", "SD1", "SD2", "SD", "LD"}
	for _, priority := range priorities {
		if streams[priority] != "" {
			return streams[priority]
		}
	}
	keys := make([]string, 0, len(streams))
	for key := range streams {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if streams[key] != "" {
			return streams[key]
		}
	}
	return ""
}

func parseDouyin(roomID string) (string, error) {
	data, err := fetchDouyinRoom(roomID)
	if err != nil {
		return "", err
	}
	if data.Room.Status != 2 {
		return "", fmt.Errorf("抖音主播当前未开播")
	}
	playURL := selectDouyinHLS(data.Room.StreamURL.HLS, data.Room.StreamURL.DefaultResolution)
	if playURL == "" {
		return "", fmt.Errorf("抖音未返回可用的 HLS 播放地址")
	}
	return playURL, nil
}

type streamCandidate struct {
	URL     string
	Bitrate int
}

func collectStreamCandidates(value any, candidates *[]streamCandidate) {
	switch typed := value.(type) {
	case map[string]any:
		if rawURL, ok := typed["url"].(string); ok {
			lowerURL := strings.ToLower(rawURL)
			if strings.Contains(lowerURL, ".flv") || strings.Contains(lowerURL, ".m3u8") {
				bitrate := 0
				switch rawBitrate := typed["bitrate"].(type) {
				case float64:
					bitrate = int(rawBitrate)
				case string:
					bitrate, _ = strconv.Atoi(rawBitrate)
				}
				*candidates = append(*candidates, streamCandidate{URL: rawURL, Bitrate: bitrate})
			}
		}
		for _, child := range typed {
			collectStreamCandidates(child, candidates)
		}
	case []any:
		for _, child := range typed {
			collectStreamCandidates(child, candidates)
		}
	}
}

func selectHighestCandidate(candidates []streamCandidate, extension string) string {
	bestIndex := -1
	for index, candidate := range candidates {
		if extension != "" && !strings.Contains(strings.ToLower(candidate.URL), extension) {
			continue
		}
		if bestIndex < 0 || candidate.Bitrate >= candidates[bestIndex].Bitrate {
			bestIndex = index
		}
	}
	if bestIndex < 0 {
		return ""
	}
	return candidates[bestIndex].URL
}

type kuaishouPageEntry struct {
	LiveStream struct {
		Caption  string          `json:"caption"`
		PlayURLs json.RawMessage `json:"playUrls"`
	} `json:"liveStream"`
	Author struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Avatar string `json:"avatar"`
	} `json:"author"`
	IsLiving bool `json:"isLiving"`
}

func findKuaishouEntry(value any) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		if _, ok := typed["liveStream"]; ok {
			return typed
		}
		for _, child := range typed {
			if found := findKuaishouEntry(child); found != nil {
				return found
			}
		}
	case []any:
		for _, child := range typed {
			if found := findKuaishouEntry(child); found != nil {
				return found
			}
		}
	}
	return nil
}

func parseKuaishouPage(body []byte) (kuaishouPageEntry, []streamCandidate, error) {
	statePattern := regexp.MustCompile(`(?s)<script>window\.__INITIAL_STATE__=(.*?);\(function\(\)\{var s;`)
	match := statePattern.FindSubmatch(body)
	if len(match) < 2 {
		return kuaishouPageEntry{}, nil, fmt.Errorf("快手页面未包含直播数据")
	}
	stateJSON := regexp.MustCompile(`\bundefined\b`).ReplaceAll(match[1], []byte("null"))
	var state any
	if err := json.Unmarshal(stateJSON, &state); err != nil {
		return kuaishouPageEntry{}, nil, fmt.Errorf("解析快手页面数据失败: %w", err)
	}
	var found map[string]any
	if root, ok := state.(map[string]any); ok {
		if liveRoom, ok := root["liveroom"].(map[string]any); ok {
			if playList, ok := liveRoom["playList"].([]any); ok && len(playList) > 0 {
				found, _ = playList[0].(map[string]any)
			}
		}
	}
	if found == nil {
		found = findKuaishouEntry(state)
	}
	if found == nil {
		return kuaishouPageEntry{}, nil, fmt.Errorf("快手主播当前未开播")
	}
	raw, _ := json.Marshal(found)
	var entry kuaishouPageEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return kuaishouPageEntry{}, nil, err
	}
	var playData any
	if len(entry.LiveStream.PlayURLs) > 0 {
		_ = json.Unmarshal(entry.LiveStream.PlayURLs, &playData)
	}
	var candidates []streamCandidate
	collectStreamCandidates(playData, &candidates)
	return entry, candidates, nil
}

func fetchKuaishouPage(roomID string) (kuaishouPageEntry, []streamCandidate, error) {
	roomID = normalizeRoomID(roomID)
	req, err := http.NewRequest(http.MethodGet, "https://live.kuaishou.com/u/"+url.PathEscape(roomID), nil)
	if err != nil {
		return kuaishouPageEntry{}, nil, err
	}
	req.Header.Set("User-Agent", desktopUserAgent)
	if cookie := strings.TrimSpace(os.Getenv("LIVE_KUAISHOU_COOKIE")); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := newLiveClient().Do(req)
	if err != nil {
		return kuaishouPageEntry{}, nil, err
	}
	body, err := readLiveResponse(resp, "快手")
	if err != nil {
		return kuaishouPageEntry{}, nil, err
	}
	return parseKuaishouPage(body)
}

func fetchKuaishouHLS(roomID, cookie string) (string, error) {
	form := url.Values{
		"source":      {"5"},
		"eid":         {normalizeRoomID(roomID)},
		"shareMethod": {"card"},
		"clientType":  {"WEB_OUTSIDE_SHARE_H5"},
	}
	endpoint := "https://livev.m.chenzhongtech.com/rest/k/live/byUser?kpn=GAME_ZONE&captchaToken="
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", mobileUserAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	resp, err := newLiveClient().Do(req)
	if err != nil {
		return "", err
	}
	body, err := readLiveResponse(resp, "快手 HLS")
	if err != nil {
		return "", err
	}
	var result struct {
		LiveStream struct {
			Living       bool   `json:"living"`
			HLSPlayURL   string `json:"hlsPlayUrl"`
			MultiHLSList []struct {
				URLs []struct {
					URL     string `json:"url"`
					Bitrate int    `json:"bitrate"`
				} `json:"urls"`
			} `json:"multiResolutionHlsPlayUrls"`
		} `json:"liveStream"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("解析快手 HLS 数据失败: %w", err)
	}
	var candidates []streamCandidate
	for _, group := range result.LiveStream.MultiHLSList {
		for _, item := range group.URLs {
			candidates = append(candidates, streamCandidate{URL: item.URL, Bitrate: item.Bitrate})
		}
	}
	if playURL := selectHighestCandidate(candidates, ".m3u8"); playURL != "" {
		return playURL, nil
	}
	if result.LiveStream.HLSPlayURL != "" {
		return result.LiveStream.HLSPlayURL, nil
	}
	return "", fmt.Errorf("快手未返回可用的 HLS 播放地址")
}

func parseKuaishou(roomID string) (string, error) {
	var hlsErr error
	if cookie := strings.TrimSpace(os.Getenv("LIVE_KUAISHOU_COOKIE")); cookie != "" {
		if playURL, err := fetchKuaishouHLS(roomID, cookie); err == nil {
			return playURL, nil
		} else {
			hlsErr = err
		}
	}
	_, candidates, err := fetchKuaishouPage(roomID)
	if err != nil {
		if hlsErr != nil {
			return "", fmt.Errorf("快手 HLS 解析失败: %v；FLV 回退失败: %w", hlsErr, err)
		}
		return "", err
	}
	if playURL := selectHighestCandidate(candidates, ".flv"); playURL != "" {
		return playURL, nil
	}
	return "", fmt.Errorf("快手主播当前未开播或未返回可用直播流")
}

// 动态解析核心分流器
func parseLiveStream(platform, roomID string) (string, error) {
	switch platform {
	case "douyin":
		return parseDouyin(roomID)
	case "kuaishou":
		return parseKuaishou(roomID)
	case "douyu":
		return parseDouyu(roomID)
	case "huya":
		return parseHuya(roomID)
	case "bilibili":
		return parseBilibili(roomID)
	}
	return "", fmt.Errorf("不支持的平台: %s", platform)
}

// 斗鱼当前网页接口稳定返回 FLV，使用公开加密参数计算匿名签名。
func parseDouyu(roomID string) (string, error) {
	const deviceID = "10000000000000000000000000001501"
	roomID = normalizeRoomID(roomID)
	headers := func(req *http.Request) {
		req.Header.Set("User-Agent", desktopUserAgent)
		req.Header.Set("Referer", "https://www.douyu.com/"+roomID)
	}

	encryptionURL := "https://www.douyu.com/wgapi/livenc/liveweb/websec/getEncryption?did=" + deviceID
	req, err := http.NewRequest(http.MethodGet, encryptionURL, nil)
	if err != nil {
		return "", err
	}
	headers(req)
	resp, err := newLiveClient().Do(req)
	if err != nil {
		return "", err
	}
	body, err := readLiveResponse(resp, "斗鱼签名")
	if err != nil {
		return "", err
	}
	var encryption struct {
		Error int `json:"error"`
		Data  struct {
			RandStr   string `json:"rand_str"`
			Key       string `json:"key"`
			EncTime   int    `json:"enc_time"`
			EncData   string `json:"enc_data"`
			IsSpecial int    `json:"is_special"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &encryption); err != nil {
		return "", err
	}
	if encryption.Error != 0 || encryption.Data.EncData == "" {
		return "", fmt.Errorf("斗鱼签名参数获取失败")
	}

	timestamp := time.Now().Unix()
	seed := encryption.Data.RandStr
	for index := 0; index < encryption.Data.EncTime; index++ {
		seed = md5Hex(seed + encryption.Data.Key)
	}
	suffix := ""
	if encryption.Data.IsSpecial != 1 {
		suffix = roomID + strconv.FormatInt(timestamp, 10)
	}
	auth := md5Hex(seed + encryption.Data.Key + suffix)
	form := url.Values{
		"enc_data": {encryption.Data.EncData},
		"tt":       {strconv.FormatInt(timestamp, 10)},
		"did":      {deviceID},
		"auth":     {auth},
		"cdn":      {""},
		"rate":     {"0"},
		"hevc":     {"0"},
		"fa":       {"0"},
		"ive":      {"0"},
	}
	playEndpoint := "https://www.douyu.com/lapi/live/getH5PlayV1/" + url.PathEscape(roomID)
	req, err = http.NewRequest(http.MethodPost, playEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	headers(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err = newLiveClient().Do(req)
	if err != nil {
		return "", err
	}
	body, err = readLiveResponse(resp, "斗鱼播放")
	if err != nil {
		return "", err
	}
	var result struct {
		Error int             `json:"error"`
		Msg   string          `json:"msg"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	if result.Error != 0 {
		return "", fmt.Errorf("斗鱼 API 报错: %s", result.Msg)
	}
	var stream struct {
		RTMPURL  string `json:"rtmp_url"`
		RTMPLive string `json:"rtmp_live"`
	}
	if err := json.Unmarshal(result.Data, &stream); err != nil {
		return "", fmt.Errorf("解析斗鱼播放数据失败: %w", err)
	}
	if stream.RTMPURL == "" || stream.RTMPLive == "" {
		return "", fmt.Errorf("斗鱼主播当前未开播或未返回可用直播流")
	}
	return strings.Replace(stream.RTMPURL+"/"+stream.RTMPLive, "http://", "https://", 1), nil
}

type huyaStreamInfo struct {
	CDNType     string `json:"sCdnType"`
	StreamName  string `json:"sStreamName"`
	FLVAntiCode string `json:"sFlvAntiCode"`
	HLSURL      string `json:"sHlsUrl"`
	HLSSuffix   string `json:"sHlsUrlSuffix"`
}

type huyaRoomData struct {
	Status int `json:"status"`
	Data   struct {
		RealLiveStatus string `json:"realLiveStatus"`
		ProfileInfo    struct {
			Nick      string `json:"nick"`
			Avatar180 string `json:"avatar180"`
			Avatar    string `json:"avatar"`
		} `json:"profileInfo"`
		LiveData struct {
			Introduction string `json:"introduction"`
		} `json:"liveData"`
		Stream struct {
			BaseStreamInfoList []huyaStreamInfo `json:"baseSteamInfoList"`
		} `json:"stream"`
	} `json:"data"`
}

func fetchHuyaRoom(roomID string) (huyaRoomData, error) {
	params := url.Values{
		"m":          {"Live"},
		"do":         {"profileRoom"},
		"roomid":     {normalizeRoomID(roomID)},
		"showSecret": {"1"},
	}
	req, err := http.NewRequest(http.MethodGet, "https://mp.huya.com/cache.php?"+params.Encode(), nil)
	if err != nil {
		return huyaRoomData{}, err
	}
	req.Header.Set("User-Agent", mobileUserAgent)
	req.Header.Set("Referer", "https://servicewechat.com/wx74767bf0b684f7d3/301/page-frame.html")
	resp, err := newLiveClient().Do(req)
	if err != nil {
		return huyaRoomData{}, err
	}
	body, err := readLiveResponse(resp, "虎牙")
	if err != nil {
		return huyaRoomData{}, err
	}
	var result huyaRoomData
	if err := json.Unmarshal(body, &result); err != nil {
		return huyaRoomData{}, err
	}
	if result.Status != http.StatusOK {
		return huyaRoomData{}, fmt.Errorf("虎牙 API 返回状态 %d", result.Status)
	}
	return result, nil
}

func selectHuyaStream(streams []huyaStreamInfo) (huyaStreamInfo, bool) {
	priorities := []string{"TX", "HW", "HS", "AL"}
	for _, priority := range priorities {
		for _, stream := range streams {
			if strings.HasPrefix(stream.CDNType, priority) && stream.HLSURL != "" && stream.StreamName != "" && stream.FLVAntiCode != "" {
				return stream, true
			}
		}
	}
	for _, stream := range streams {
		if stream.HLSURL != "" && stream.StreamName != "" && stream.FLVAntiCode != "" {
			return stream, true
		}
	}
	return huyaStreamInfo{}, false
}

func buildHuyaHLS(stream huyaStreamInfo, now time.Time, uid, uuid int64) (string, error) {
	query, err := url.ParseQuery(stream.FLVAntiCode)
	if err != nil {
		return "", err
	}
	fm, err := base64.StdEncoding.DecodeString(query.Get("fm"))
	if err != nil {
		return "", fmt.Errorf("解码虎牙 fm 参数失败: %w", err)
	}
	prefix := strings.Split(string(fm), "_")[0]
	ctype := query.Get("ctype")
	fs := query.Get("fs")
	if prefix == "" || ctype == "" || fs == "" {
		return "", fmt.Errorf("虎牙 anti-code 缺少必要参数")
	}
	const clientType = int64(100)
	const sdkVersion = int64(2403051612)
	timestampMillis := now.Unix() * 1000
	sdkSID := timestampMillis
	sequenceID := uid + sdkSID
	wsTime := strconv.FormatInt((timestampMillis+110624)/1000, 16)
	secretHash := md5Hex(fmt.Sprintf("%d|%s|%d", sequenceID, ctype, clientType))
	wsSecret := md5Hex(fmt.Sprintf("%s_%d_%s_%s_%s", prefix, uid, stream.StreamName, secretHash, wsTime))
	antiCode := url.Values{
		"wsSecret": {wsSecret},
		"wsTime":   {wsTime},
		"seqid":    {strconv.FormatInt(sequenceID, 10)},
		"ctype":    {ctype},
		"ver":      {"1"},
		"fs":       {fs},
		"uuid":     {strconv.FormatInt(uuid, 10)},
		"u":        {strconv.FormatInt(uid, 10)},
		"t":        {strconv.FormatInt(clientType, 10)},
		"sv":       {strconv.FormatInt(sdkVersion, 10)},
		"sdk_sid":  {strconv.FormatInt(sdkSID, 10)},
		"codec":    {"264"},
	}
	baseURL := strings.Replace(stream.HLSURL, "http://", "https://", 1)
	suffix := stream.HLSSuffix
	if suffix == "" {
		suffix = "m3u8"
	}
	return fmt.Sprintf("%s/%s.%s?%s&ratio=", baseURL, stream.StreamName, suffix, antiCode.Encode()), nil
}

// 虎牙原始 anti-code 不能直接播放，需要按当前时间重新计算。
func parseHuya(roomID string) (string, error) {
	data, err := fetchHuyaRoom(roomID)
	if err != nil {
		return "", err
	}
	if data.Data.RealLiveStatus != "ON" {
		return "", fmt.Errorf("虎牙主播当前未开播")
	}
	stream, ok := selectHuyaStream(data.Data.Stream.BaseStreamInfoList)
	if !ok {
		return "", fmt.Errorf("虎牙没有可用的 HLS 线路")
	}
	now := time.Now()
	uid := int64(1400000000000) + now.UnixNano()%10000000
	uuid := ((now.Unix()*1000)%10000000000*1000 + int64(now.Nanosecond()%1000)) % 4294967295
	return buildHuyaHLS(stream, now, uid, uuid)
}

type bilibiliCodec struct {
	CodecName string `json:"codec_name"`
	CurrentQN int    `json:"current_qn"`
	BaseURL   string `json:"base_url"`
	URLInfo   []struct {
		Host  string `json:"host"`
		Extra string `json:"extra"`
	} `json:"url_info"`
}

type bilibiliFormat struct {
	FormatName string          `json:"format_name"`
	Codec      []bilibiliCodec `json:"codec"`
}

type bilibiliStream struct {
	ProtocolName string           `json:"protocol_name"`
	Format       []bilibiliFormat `json:"format"`
}

func selectBilibiliURL(streams []bilibiliStream) string {
	priorities := []struct {
		Protocol string
		Format   string
		Codec    string
	}{
		{Protocol: "http_hls", Format: "ts", Codec: "avc"},
		{Protocol: "http_hls", Format: "fmp4", Codec: "avc"},
		{Protocol: "http_stream", Format: "flv", Codec: "avc"},
	}
	for _, priority := range priorities {
		var selected *bilibiliCodec
		for _, stream := range streams {
			if stream.ProtocolName != priority.Protocol {
				continue
			}
			for _, format := range stream.Format {
				if format.FormatName != priority.Format {
					continue
				}
				for index := range format.Codec {
					codec := &format.Codec[index]
					if codec.CodecName == priority.Codec && len(codec.URLInfo) > 0 && (selected == nil || codec.CurrentQN > selected.CurrentQN) {
						selected = codec
					}
				}
			}
		}
		if selected != nil {
			return selected.URLInfo[0].Host + selected.BaseURL + selected.URLInfo[0].Extra
		}
	}
	return ""
}

// B 站优先返回对浏览器兼容性最好的 AVC HLS。
func parseBilibili(roomID string) (string, error) {
	params := url.Values{
		"room_id":  {normalizeRoomID(roomID)},
		"protocol": {"0,1"},
		"format":   {"0,1,2"},
		"codec":    {"0,1"},
		"qn":       {"10000"},
		"platform": {"h5"},
		"ptype":    {"8"},
	}
	endpoint := "https://api.live.bilibili.com/xlive/web-room/v2/index/getRoomPlayInfo?" + params.Encode()
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", desktopUserAgent)
	req.Header.Set("Origin", "https://live.bilibili.com")
	req.Header.Set("Referer", "https://live.bilibili.com/")
	resp, err := newLiveClient().Do(req)
	if err != nil {
		return "", err
	}
	body, err := readLiveResponse(resp, "B站")
	if err != nil {
		return "", err
	}
	var result struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			LiveStatus  int `json:"live_status"`
			PlayURLInfo struct {
				PlayURL struct {
					Stream []bilibiliStream `json:"stream"`
				} `json:"playurl"`
			} `json:"playurl_info"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	if result.Code != 0 {
		return "", fmt.Errorf("B站 API 报错: %s", result.Message)
	}
	if result.Data.LiveStatus == 0 {
		return "", fmt.Errorf("B站主播当前未开播")
	}
	playURL := selectBilibiliURL(result.Data.PlayURLInfo.PlayURL.Stream)
	if playURL == "" {
		return "", fmt.Errorf("B站未返回可用的 AVC HLS/FLV 直播流")
	}
	return playURL, nil
}

// ==================== 3. 联想头像与信息抓取逻辑 ====================

func fetchDouyinInfo(roomID string) (string, string, error) {
	data, err := fetchDouyinRoom(roomID)
	if err != nil {
		return "", "", err
	}
	user := data.Room.Owner
	if user.Nickname == "" {
		user = data.Anchor
	}
	avatar := ""
	if len(user.AvatarThumb.URLList) > 0 {
		avatar = user.AvatarThumb.URLList[0]
	} else if len(data.Room.Cover.URLList) > 0 {
		avatar = data.Room.Cover.URLList[0]
	}
	return avatar, user.Nickname, nil
}

func fetchKuaishouInfo(roomID string) (string, string, error) {
	entry, _, err := fetchKuaishouPage(roomID)
	if err != nil {
		return "", "", err
	}
	return entry.Author.Avatar, entry.Author.Name, nil
}

// 斗鱼主播基本信息免签拉取
func fetchDouyuInfo(roomID string) (string, string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	u := fmt.Sprintf("https://open.douyucdn.cn/api/RoomApi/room/%s", roomID)
	req, _ := http.NewRequest("GET", u, nil)

	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	var result struct {
		Error int `json:"error"`
		Data  struct {
			RoomName  string `json:"room_name"`
			OwnerName string `json:"owner_name"`
			Avatar    string `json:"avatar"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", err
	}

	if result.Error != 0 {
		return "", "", fmt.Errorf("房间不存在或解析失败")
	}

	return result.Data.Avatar, result.Data.OwnerName, nil
}

func fetchHuyaInfo(roomID string) (string, string, error) {
	data, err := fetchHuyaRoom(roomID)
	if err != nil {
		return "", "", err
	}
	avatar := data.Data.ProfileInfo.Avatar180
	if avatar == "" {
		avatar = data.Data.ProfileInfo.Avatar
	}
	return avatar, data.Data.ProfileInfo.Nick, nil
}

// B站主播基本信息免签拉取
func fetchBilibiliInfo(roomID string) (string, string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	u := fmt.Sprintf("https://api.live.bilibili.com/room/v1/Room/get_info?room_id=%s", roomID)
	req, _ := http.NewRequest("GET", u, nil)

	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	var result struct {
		Code int `json:"code"`
		Data struct {
			UID      int    `json:"uid"`
			Title    string `json:"title"`
			CoverURL string `json:"user_cover"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", err
	}

	if result.Code != 0 {
		return "", "", fmt.Errorf("B站房间未找到")
	}

	// 进一步获取 B站主播昵称与头像
	uInfo := fmt.Sprintf("https://api.live.bilibili.com/live_user/v1/UserInfo/get_anchor_in_room?roomid=%s", roomID)
	req2, _ := http.NewRequest("GET", uInfo, nil)
	resp2, err2 := client.Do(req2)
	if err2 == nil {
		defer resp2.Body.Close()
		var res2 struct {
			Code int `json:"code"`
			Data struct {
				Info struct {
					Face  textOrInt `json:"face"`
					Uname string    `json:"uname"`
				} `json:"info"`
			} `json:"data"`
		}
		if json.NewDecoder(resp2.Body).Decode(&res2) == nil && res2.Code == 0 {
			faceStr := string(res2.Data.Info.Face)
			if faceStr != "" {
				return faceStr, res2.Data.Info.Uname, nil
			}
		}
	}

	return result.Data.CoverURL, "B站主播", nil
}

// 解决B站API中face字段可能返回string或int的类型兼容处理
type textOrInt string

func (t *textOrInt) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*t = textOrInt(s)
		return nil
	}
	var i int
	if err := json.Unmarshal(b, &i); err == nil {
		*t = textOrInt(fmt.Sprintf("%d", i))
		return nil
	}
	return nil
}

// ==================== 4. 本地 STRM 与海报文件同步核心 ====================

func syncStrmDirectory(config LiveConfig, baseURL string, targetOutDir string) {
	log.Printf("⏳ 开始同步 STRM 文件目录，使用网关网基址: %s", baseURL)

	// 1. 读取旧的目录文件
	files, err := os.ReadDir(targetOutDir)
	if err == nil {
		for _, f := range files {
			if !f.IsDir() && (strings.HasSuffix(f.Name(), ".strm") || strings.HasSuffix(f.Name(), ".jpg")) {
				os.Remove(filepath.Join(targetOutDir, f.Name()))
			}
		}
	}

	// 2. 遍历手动配置列表
	for _, item := range config.ManualList {
		safeName := sanitizeFilename(item.Name)
		strmName := fmt.Sprintf("[手动]%s.strm", safeName)
		jpgName := fmt.Sprintf("[手动]%s.jpg", safeName)

		// 写入 strm 文件
		strmContent := fmt.Sprintf("%s/play?id=%s&type=manual", baseURL, url.QueryEscape(item.Name))
		os.WriteFile(filepath.Join(targetOutDir, strmName), []byte(strmContent), 0644)

		// 下载或写入本地海报
		if item.Logo != "" {
			go downloadImage(item.Logo, filepath.Join(targetOutDir, jpgName))
		}
	}

	// 3. 遍历动态配置列表
	for _, item := range config.AutoList {
		safeName := sanitizeFilename(item.Name)
		platMap := map[string]string{
			"douyin": "抖音", "kuaishou": "快手", "douyu": "斗鱼", "huya": "虎牙", "bilibili": "B站",
		}
		plat := platMap[item.Platform]
		if plat == "" {
			plat = item.Platform
		}

		strmName := fmt.Sprintf("[%s]%s.strm", plat, safeName)
		jpgName := fmt.Sprintf("[%s]%s.jpg", plat, safeName)

		// 写入 strm 文件
		strmContent := fmt.Sprintf("%s/play?id=%s&type=auto", baseURL, url.QueryEscape(item.Name))
		os.WriteFile(filepath.Join(targetOutDir, strmName), []byte(strmContent), 0644)

		// 下载或写入本地海报
		if item.Logo != "" {
			go downloadImage(item.Logo, filepath.Join(targetOutDir, jpgName))
		}
	}

	log.Println("✅ STRM 文件目录同步完成！")
}

// 下载图片并写入本地
func downloadImage(urlStr, savePath string) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest("GET", urlStr, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	out, err := os.Create(savePath)
	if err != nil {
		return
	}
	defer out.Close()

	_, _ = io.Copy(out, resp.Body)
}

// 过滤掉非法的文件字符
func sanitizeFilename(name string) string {
	reg := regexp.MustCompile(`[\\/:*?"<>|]`)
	return reg.ReplaceAllString(name, "_")
}

// 本地静态海报图片流分发接口，彻底绕过各大直播平台防盗链限制
func handleGetLogo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "缺少 name 参数", http.StatusBadRequest)
		return
	}

	safeName := sanitizeFilename(name)

	// 智能多路径合并查找海报图片
	dirs := []string{"./strm_test", strmOutDir}
	for _, dir := range dirs {
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if !f.IsDir() && strings.HasSuffix(f.Name(), ".jpg") && strings.Contains(f.Name(), safeName) {
				imgPath := filepath.Join(dir, f.Name())
				http.ServeFile(w, r, imgPath)
				return
			}
		}
	}

	http.Error(w, "海报图片尚未下载或已下线", http.StatusNotFound)
}
