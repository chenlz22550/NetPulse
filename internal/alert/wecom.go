package alert

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"netping/internal/config"
)

// 企业微信群机器人 markdown 消息最大 4096 字节。
const wecomMaxBytes = 4000

// SendWeCom 通过企业微信群机器人 webhook 发送 markdown 消息。
func SendWeCom(cfg config.AlertConfig, content string) error {
	if cfg.WebhookURL == "" {
		return errors.New("未配置 webhook 地址")
	}
	body, _ := json.Marshal(map[string]interface{}{
		"msgtype":  "markdown",
		"markdown": map[string]string{"content": content},
	})
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	if cfg.ProxyURL != "" {
		u, err := url.Parse(cfg.ProxyURL)
		if err != nil {
			return fmt.Errorf("代理地址无效: %w", err)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	if cfg.TLSSkipVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // 用户显式开启
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: tr}
	defer tr.CloseIdleConnections()

	resp, err := client.Post(cfg.WebhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("请求企业微信失败: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("企业微信返回 HTTP %d: %s", resp.StatusCode, string(data))
	}
	var r struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("无法解析企业微信响应: %s", string(data))
	}
	if r.ErrCode != 0 {
		return fmt.Errorf("企业微信返回错误 %d: %s", r.ErrCode, r.ErrMsg)
	}
	return nil
}
