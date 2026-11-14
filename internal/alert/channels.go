package alert

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"netping/internal/config"
)

var fontTags = regexp.MustCompile(`</?font[^>]*>`)

// SendNotifications fans out independently; errors identify failed channels without exposing URLs.
func SendNotifications(cfg config.AlertConfig, content string) error {
	if err := cfg.ValidateChannels(); err != nil {
		return err
	}
	if !cfg.HasChannels() {
		return errors.New("未配置通知渠道")
	}
	type destination struct{ name, kind, url, secret string }
	destinations := []destination{
		{"企业微信", "wecom", cfg.WebhookURL, ""},
		{"钉钉", "dingtalk", cfg.DingTalkURL, cfg.DingTalkSecret},
		{"飞书", "feishu", cfg.FeishuURL, cfg.FeishuSecret},
		{"Slack", "slack", cfg.SlackURL, ""},
		{"Webhook", "webhook", cfg.GenericWebhookURL, ""},
	}
	results := make([]error, len(destinations))
	var wg sync.WaitGroup
	for i, d := range destinations {
		if d.url == "" {
			continue
		}
		wg.Add(1)
		go func(i int, d destination) {
			defer wg.Done()
			if err := sendChannel(cfg, d.kind, d.url, d.secret, content); err != nil {
				results[i] = fmt.Errorf("%s: %w", d.name, err)
			}
		}(i, d)
	}
	wg.Wait()
	return errors.Join(results...)
}

func channelSignature(key, message string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(message))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func sendChannel(cfg config.AlertConfig, kind, endpoint, secret, content string) error {
	text := fontTags.ReplaceAllString(content, "")
	var payload interface{}
	switch kind {
	case "wecom":
		payload = map[string]interface{}{"msgtype": "markdown", "markdown": map[string]string{"content": content}}
	case "dingtalk":
		payload = map[string]interface{}{"msgtype": "markdown", "markdown": map[string]string{"title": cfg.SiteTitle, "text": text}}
		if secret != "" {
			ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
			u, err := url.Parse(endpoint)
			if err != nil {
				return errors.New("Webhook 地址无效")
			}
			q := u.Query()
			q.Set("timestamp", ts)
			q.Set("sign", channelSignature(secret, ts+"\n"+secret))
			u.RawQuery = q.Encode()
			endpoint = u.String()
		}
	case "feishu":
		body := map[string]interface{}{"msg_type": "text", "content": map[string]string{"text": text}}
		if secret != "" {
			ts := strconv.FormatInt(time.Now().Unix(), 10)
			body["timestamp"] = ts
			body["sign"] = channelSignature(ts+"\n"+secret, "")
		}
		payload = body
	case "slack":
		payload = map[string]string{"text": text}
	default:
		payload = map[string]string{"title": cfg.SiteTitle, "text": text}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	tr := &http.Transport{Proxy: http.ProxyFromEnvironment, TLSHandshakeTimeout: 10 * time.Second}
	defer tr.CloseIdleConnections()
	if cfg.ProxyURL != "" {
		u, err := url.Parse(cfg.ProxyURL)
		if err != nil {
			return errors.New("代理地址无效")
		}
		tr.Proxy = http.ProxyURL(u)
	}
	if cfg.TLSSkipVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	} //nolint:gosec
	client := &http.Client{Transport: tr, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return errors.New("请求失败，请检查网络、地址、代理或证书")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return errors.New("读取响应失败")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if kind == "webhook" {
		return nil
	}
	if kind == "slack" {
		if strings.TrimSpace(string(data)) != "ok" {
			return errors.New("Slack 未确认接收消息")
		}
		return nil
	}
	var result struct {
		ErrCode    *int `json:"errcode"`
		Code       *int `json:"code"`
		StatusCode *int `json:"StatusCode"`
	}
	if json.Unmarshal(data, &result) != nil {
		return errors.New("无法解析渠道响应")
	}
	code := result.ErrCode
	if kind == "feishu" {
		code = result.Code
		if code == nil {
			code = result.StatusCode
		}
	}
	if code == nil {
		return errors.New("响应缺少发送结果")
	}
	if *code != 0 {
		return fmt.Errorf("渠道返回错误 %d", *code)
	}
	return nil
}
