package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"netping/internal/config"
)

// 网页登录：密码登录后发一个 30 天有效的签名 Cookie。
// Cookie 内容为“到期时间.签名”，签名密钥保存在 data/session.key，并把当前密码摘要算进签名，
// 因此程序重启后登录仍然有效，修改密码后所有已登录的浏览器都要重新登录。

const (
	sessionTTL    = 30 * 24 * time.Hour
	pbkdf2Iter    = 120000
	minPasswordLn = 6
	maxFails      = 5 // 同一来源连续失败多少次后开始锁定
)

// 不需要登录即可访问的路径。
var publicPaths = map[string]bool{
	"/login.html":      true,
	"/api/auth/state":  true,
	"/api/auth/login":  true,
	"/api/auth/setup":  true,
	"/api/auth/logout": true,
}

type loginFail struct {
	n     int
	until time.Time
}

type authState struct {
	keyOnce sync.Once
	key     []byte
	mu      sync.Mutex
	fails   map[string]*loginFail
}

// publicConfig 去掉密码相关字段后再发给网页。
func publicConfig(c config.Config) config.Config {
	c.Auth.Password = ""
	c.Auth.PasswordHash = ""
	c.AI.APIKey = ""
	return c
}

// ---- 密码摘要（PBKDF2-HMAC-SHA256） ----

func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hLen := prf.Size()
	blocks := (keyLen + hLen - 1) / hLen
	out := make([]byte, 0, blocks*hLen)
	buf := make([]byte, 4)
	for b := 1; b <= blocks; b++ {
		prf.Reset()
		prf.Write(salt)
		buf[0], buf[1], buf[2], buf[3] = byte(b>>24), byte(b>>16), byte(b>>8), byte(b)
		prf.Write(buf)
		u := prf.Sum(nil)
		t := append([]byte(nil), u...)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum := pbkdf2SHA256([]byte(pw), salt, pbkdf2Iter, 32)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter, enc.EncodeToString(salt), enc.EncodeToString(sum)), nil
}

func checkPassword(a config.Auth, pw string) bool {
	if pw == "" {
		return false
	}
	if a.PasswordHash != "" {
		parts := strings.Split(a.PasswordHash, "$")
		if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
			return false
		}
		iter, err := strconv.Atoi(parts[1])
		if err != nil || iter < 1 || iter > 10000000 {
			return false
		}
		enc := base64.RawStdEncoding
		salt, err1 := enc.DecodeString(parts[2])
		want, err2 := enc.DecodeString(parts[3])
		if err1 != nil || err2 != nil || len(want) == 0 {
			return false
		}
		return subtle.ConstantTimeCompare(pbkdf2SHA256([]byte(pw), salt, iter, len(want)), want) == 1
	}
	return a.Password != "" && subtle.ConstantTimeCompare([]byte(pw), []byte(a.Password)) == 1
}

// ---- 会话 Cookie ----

func (s *Server) sessionKey() []byte {
	s.auth.keyOnce.Do(func() {
		path := filepath.Join(s.BaseDir, "data", "session.key")
		if b, err := os.ReadFile(path); err == nil {
			if k, err := hex.DecodeString(strings.TrimSpace(string(b))); err == nil && len(k) >= 32 {
				s.auth.key = k
				return
			}
		}
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			panic(err)
		}
		s.auth.key = k
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(hex.EncodeToString(k)), 0o600); err != nil {
			log.Printf("保存登录密钥失败（重启后需重新登录）: %v", err)
		}
	})
	return s.auth.key
}

// cookieName 带上端口号，避免同一台电脑上不同端口的实例互相覆盖 Cookie。
func (s *Server) cookieName() string {
	_, port, err := net.SplitHostPort(s.Cfg.Get().Listen)
	if err != nil || port == "" {
		return "netping_session"
	}
	return "netping_session_" + port
}

func credFingerprint(a config.Auth) string {
	if a.PasswordHash != "" {
		return "h:" + a.PasswordHash
	}
	return "p:" + a.Password
}

func (s *Server) sign(exp int64, a config.Auth) string {
	m := hmac.New(sha256.New, s.sessionKey())
	fmt.Fprintf(m, "%d|%s", exp, credFingerprint(a))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Server) setSession(w http.ResponseWriter, a config.Auth) {
	exp := time.Now().Add(sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(), Value: strconv.FormatInt(exp.Unix(), 10) + "." + s.sign(exp.Unix(), a),
		Path: "/", Expires: exp, MaxAge: int(sessionTTL / time.Second), HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

func (s *Server) validSession(r *http.Request, a config.Auth) bool {
	c, err := r.Cookie(s.cookieName())
	if err != nil {
		return false
	}
	i := strings.IndexByte(c.Value, '.')
	if i <= 0 {
		return false
	}
	exp, err := strconv.ParseInt(c.Value[:i], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(c.Value[i+1:]), []byte(s.sign(exp, a)))
}

// authorized 判断请求是否已登录：有效的会话 Cookie，或 HTTP Basic 认证（供脚本 / MCP 客户端）。
func (s *Server) authorized(r *http.Request, a config.Auth) bool {
	if !a.HasPassword() {
		return false
	}
	if s.validSession(r, a) {
		return true
	}
	if u, p, ok := r.BasicAuth(); ok {
		ip := clientAddr(r)
		if s.lockedFor(ip) > 0 {
			return false
		}
		good := (a.Username == "" || subtle.ConstantTimeCompare([]byte(u), []byte(a.Username)) == 1) && checkPassword(a, p)
		if !good {
			s.noteLogin(ip, false)
		}
		return good
	}
	return false
}

// ---- 登录失败限制 ----

func (s *Server) lockedFor(ip string) time.Duration {
	s.auth.mu.Lock()
	defer s.auth.mu.Unlock()
	if f := s.auth.fails[ip]; f != nil {
		if d := time.Until(f.until); d > 0 {
			return d
		}
	}
	return 0
}

func (s *Server) noteLogin(ip string, ok bool) {
	s.auth.mu.Lock()
	defer s.auth.mu.Unlock()
	if s.auth.fails == nil {
		s.auth.fails = map[string]*loginFail{}
	}
	if ok {
		delete(s.auth.fails, ip)
		return
	}
	f := s.auth.fails[ip]
	if f == nil {
		f = &loginFail{}
		s.auth.fails[ip] = f
	}
	f.n++
	if f.n >= maxFails {
		// 第 5 次失败锁 1 分钟，之后每次翻倍，最长 15 分钟
		d := time.Minute << uint(minInt(f.n-maxFails, 4))
		if d > 15*time.Minute {
			d = 15 * time.Minute
		}
		f.until = time.Now().Add(d)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func isLoopback(r *http.Request) bool {
	ip := net.ParseIP(clientAddr(r))
	return ip != nil && ip.IsLoopback()
}

func validNewPassword(pw string) error {
	if len([]rune(pw)) < minPasswordLn {
		return badRequest("密码至少 %d 位", minPasswordLn)
	}
	if len(pw) > 200 {
		return badRequest("密码太长")
	}
	return nil
}

// ---- 接口 ----

func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request, arg string) error {
	cfg := s.Cfg.Get()
	a := cfg.Auth
	ip := clientAddr(r)
	var req struct {
		Password    string `json:"password"`
		OldPassword string `json:"old_password"`
	}
	if r.Method == http.MethodPost && r.ContentLength != 0 {
		if err := decode(r, &req); err != nil {
			return err
		}
	}
	switch {
	case arg == "state" && r.Method == http.MethodGet:
		writeJSON(w, map[string]interface{}{
			"password_set": a.HasPassword(), "logged_in": s.authorized(r, a),
			"can_setup": !a.HasPassword() && isLoopback(r), "ttl_days": int(sessionTTL / (24 * time.Hour)),
			"version": Version,
		})
		return nil

	case arg == "login" && r.Method == http.MethodPost:
		if !a.HasPassword() {
			return badRequest("尚未设置登录密码")
		}
		if d := s.lockedFor(ip); d > 0 {
			return &apiError{status: http.StatusTooManyRequests, msg: fmt.Sprintf("密码错误次数过多，请 %d 秒后再试", int(d.Seconds())+1)}
		}
		if !checkPassword(a, req.Password) {
			s.noteLogin(ip, false)
			log.Printf("网页登录失败（来源 %s）", ip)
			time.Sleep(400 * time.Millisecond)
			return &apiError{status: http.StatusUnauthorized, msg: "密码错误"}
		}
		s.noteLogin(ip, true)
		s.setSession(w, a)
		log.Printf("网页登录成功（来源 %s）", ip)
		writeJSON(w, map[string]interface{}{"ok": true})
		return nil

	case arg == "setup" && r.Method == http.MethodPost:
		if a.HasPassword() {
			return badRequest("已经设置过登录密码，请直接登录")
		}
		if !isLoopback(r) {
			return &apiError{status: http.StatusForbidden, msg: "首次设置密码只能在运行 netPing 的电脑上操作"}
		}
		if err := validNewPassword(req.Password); err != nil {
			return err
		}
		na, err := s.savePassword(req.Password)
		if err != nil {
			return err
		}
		s.setSession(w, na)
		log.Printf("已设置网页登录密码（来源 %s）", ip)
		writeJSON(w, map[string]interface{}{"ok": true})
		return nil

	case arg == "logout" && r.Method == http.MethodPost:
		s.clearSession(w)
		writeJSON(w, map[string]interface{}{"ok": true})
		return nil

	case arg == "password" && r.Method == http.MethodPost:
		// 已经过 withAuth 校验
		if d := s.lockedFor(ip); d > 0 {
			return &apiError{status: http.StatusTooManyRequests, msg: fmt.Sprintf("密码错误次数过多，请 %d 秒后再试", int(d.Seconds())+1)}
		}
		if !checkPassword(a, req.OldPassword) {
			s.noteLogin(ip, false)
			return badRequest("当前密码不正确")
		}
		if err := validNewPassword(req.Password); err != nil {
			return err
		}
		na, err := s.savePassword(req.Password)
		if err != nil {
			return err
		}
		s.setSession(w, na)
		log.Printf("网页登录密码已修改（来源 %s），其他浏览器需重新登录", ip)
		writeJSON(w, map[string]interface{}{"ok": true})
		return nil
	}
	return notFound("接口")
}

func (s *Server) savePassword(pw string) (config.Auth, error) {
	h, err := hashPassword(pw)
	if err != nil {
		return config.Auth{}, err
	}
	c := s.Cfg.Get()
	c.Auth.Password = ""
	c.Auth.PasswordHash = h
	if err := s.Cfg.Update(c); err != nil {
		return config.Auth{}, errors.New("保存密码失败: " + err.Error())
	}
	return c.Auth, nil
}
