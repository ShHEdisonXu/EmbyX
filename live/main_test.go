package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSelectDouyinHLS(t *testing.T) {
	streams := map[string]string{
		"FULL_HD1": "https://example.com/full/index.m3u8",
		"HD1":      "https://example.com/hd/index.m3u8",
	}
	if got := selectDouyinHLS(streams, "HD1"); got != streams["HD1"] {
		t.Fatalf("default resolution not selected: %q", got)
	}
	if got := selectDouyinHLS(streams, "missing"); got != streams["FULL_HD1"] {
		t.Fatalf("quality fallback not selected: %q", got)
	}
}

func TestSelectHighestCandidate(t *testing.T) {
	candidates := []streamCandidate{
		{URL: "https://example.com/low.flv", Bitrate: 800},
		{URL: "https://example.com/live.m3u8", Bitrate: 4000},
		{URL: "https://example.com/high.flv", Bitrate: 2000},
	}
	if got := selectHighestCandidate(candidates, ".flv"); got != candidates[2].URL {
		t.Fatalf("highest FLV bitrate not selected: %q", got)
	}
}

func TestParseDouyinPage(t *testing.T) {
	payload := `[{"state":{"roomStore":{"roomInfo":{"room":{"id_str":"1","status":2,"owner":{"nickname":"主播"},"stream_url":{"default_resolution":"HD1","hls_pull_url_map":{"HD1":"https://example.com/live.m3u8"}}}}}}}]`
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`<script>__pace_f.push(` + string(encoded) + `)</script>`)
	data, err := parseDouyinPage(body)
	if err != nil {
		t.Fatal(err)
	}
	if data.Room.Status != 2 || data.Room.Owner.Nickname != "主播" || data.Room.StreamURL.HLS["HD1"] == "" {
		t.Fatalf("unexpected douyin room data: %+v", data.Room)
	}
}

func TestParseKuaishouPage(t *testing.T) {
	body := []byte(`<script>window.__INITIAL_STATE__={"liveroom":{"playList":[{"liveStream":{"caption":"test","playUrls":{"h264":{"adaptationSet":{"representation":[{"url":"https://example.com/low.flv","bitrate":800},{"url":"https://example.com/high.flv","bitrate":2000}]}}}},"author":{"name":"主播","avatar":"https://example.com/avatar.jpg"},"unused":undefined}]}};(function(){var s;</script>`)
	entry, candidates, err := parseKuaishouPage(body)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Author.Name != "主播" || selectHighestCandidate(candidates, ".flv") != "https://example.com/high.flv" {
		t.Fatalf("unexpected kuaishou data: entry=%+v candidates=%+v", entry, candidates)
	}
}

func TestBuildHuyaHLS(t *testing.T) {
	fm := base64.StdEncoding.EncodeToString([]byte("prefix_$0_$1_$2_$3"))
	stream := huyaStreamInfo{
		StreamName:  "stream-name",
		FLVAntiCode: url.Values{"fm": {fm}, "ctype": {"tars_mp"}, "fs": {"bgct"}}.Encode(),
		HLSURL:      "http://tx.hls.huya.com/src",
		HLSSuffix:   "m3u8",
	}
	got, err := buildHuyaHLS(stream, time.Unix(1700000000, 0), 1400000000001, 12345)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != "https" || parsed.Path != "/src/stream-name.m3u8" {
		t.Fatalf("unexpected HLS URL: %s", got)
	}
	query := parsed.Query()
	for _, key := range []string{"wsSecret", "wsTime", "seqid", "ctype", "fs", "uuid", "u", "sdk_sid", "codec"} {
		if query.Get(key) == "" {
			t.Fatalf("missing signed query parameter %q in %s", key, got)
		}
	}
	if query.Get("codec") != "264" || !strings.Contains(got, "ratio=") {
		t.Fatalf("unexpected codec or ratio parameter: %s", got)
	}
}

func TestSelectBilibiliURLPrefersAVCHLS(t *testing.T) {
	fixture := `[
		{"protocol_name":"http_stream","format":[{"format_name":"flv","codec":[{"codec_name":"avc","current_qn":10000,"base_url":"/live.flv","url_info":[{"host":"https://flv.example.com","extra":"?token=1"}]}]}]},
		{"protocol_name":"http_hls","format":[
			{"format_name":"fmp4","codec":[{"codec_name":"avc","current_qn":10000,"base_url":"/fallback.m3u8","url_info":[{"host":"https://hls.example.com","extra":"?token=2"}]}]},
			{"format_name":"ts","codec":[
				{"codec_name":"hevc","current_qn":10000,"base_url":"/hevc.m3u8","url_info":[{"host":"https://hls.example.com","extra":"?token=3"}]},
				{"codec_name":"avc","current_qn":250,"base_url":"/avc.m3u8","url_info":[{"host":"https://hls.example.com","extra":"?token=4"}]}
			]}
		]}
	]`
	var streams []bilibiliStream
	if err := json.Unmarshal([]byte(fixture), &streams); err != nil {
		t.Fatal(err)
	}
	want := "https://hls.example.com/avc.m3u8?token=4"
	if got := selectBilibiliURL(streams); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestPlayHandlerRoutesByActualFormat(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/x-flv")
		_, _ = w.Write([]byte("flv-data"))
	}))
	defer upstream.Close()

	config := LiveConfig{ManualList: []ManualChannel{
		{Name: "handler-hls-check", URL: "https://example.com/live.m3u8"},
		{Name: "handler-flv-check", URL: upstream.URL + "/live.flv"},
	}}
	rawConfig, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	configFile := t.TempDir() + "/live.json"
	if err := os.WriteFile(configFile, rawConfig, 0600); err != nil {
		t.Fatal(err)
	}
	previousConfigPath := configPath
	configPath = configFile
	defer func() { configPath = previousConfigPath }()

	hlsRequest := httptest.NewRequest(http.MethodGet, "/play?id=handler-hls-check&type=manual", nil)
	hlsResponse := httptest.NewRecorder()
	handlePlayRedirect(hlsResponse, hlsRequest)
	if hlsResponse.Code != http.StatusFound || hlsResponse.Header().Get("Location") != "https://example.com/live.m3u8" {
		t.Fatalf("HLS should redirect: status=%d location=%q", hlsResponse.Code, hlsResponse.Header().Get("Location"))
	}

	flvRequest := httptest.NewRequest(http.MethodGet, "/play?id=handler-flv-check&type=manual", nil)
	flvRequest.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X)")
	flvResponse := httptest.NewRecorder()
	handlePlayRedirect(flvResponse, flvRequest)
	if flvResponse.Code != http.StatusOK || flvResponse.Body.String() != "flv-data" {
		t.Fatalf("FLV should be relayed even for iPhone: status=%d body=%q", flvResponse.Code, flvResponse.Body.String())
	}
}
