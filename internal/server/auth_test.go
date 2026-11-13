package server

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"netping/internal/config"
	"netping/internal/topology"
)

func authPost(t *testing.T, c *http.Client, ts *httptest.Server, path, body string) (int, map[string]interface{}) {
	t.Helper()
	resp, err := c.Post(ts.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func newClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func getStatus(t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Location")
}

func TestPasswordHash(t *testing.T) {
	h, err := hashPassword("hello123")
	if err != nil || !strings.HasPrefix(h, "pbkdf2-sha256$") {
		t.Fatalf("hash %q %v", h, err)
	}
	a := config.Auth{PasswordHash: h}
	if !checkPassword(a, "hello123") || checkPassword(a, "hello124") || checkPassword(a, "") {
		t.Fatal("hash check")
	}
	if !checkPassword(config.Auth{Password: "plain1"}, "plain1") || checkPassword(config.Auth{Password: "plain1"}, "plain2") {
		t.Fatal("plain check")
	}
	// RFC 7914 测试向量：PBKDF2-HMAC-SHA256("passwd", "salt", 1, 64)
	got := pbkdf2SHA256([]byte("passwd"), []byte("salt"), 1, 16)
	want := []byte{0x55, 0xac, 0x04, 0x6e, 0x56, 0xe3, 0x08, 0x9f, 0xec, 0x16, 0x91, 0xc2, 0x25, 0x44, 0xb6, 0x05}
	if string(got) != string(want) {
		t.Fatalf("pbkdf2 vector: %x", got)
	}
}

func TestWebLogin(t *testing.T) {
	s, ts := newTestServer(t)
	c := newClient()

	// 未登录：网页跳到登录页，接口返回 401，登录页本身可访问
	if code, loc := getStatus(t, c, ts.URL+"/"); code != http.StatusFound || loc != "/login.html" {
		t.Fatalf("page: %d %s", code, loc)
	}
	if code, _ := getStatus(t, c, ts.URL+"/api/config"); code != http.StatusUnauthorized {
		t.Fatalf("api: %d", code)
	}
	if code, _ := getStatus(t, c, ts.URL+"/login.html"); code != 200 {
		t.Fatalf("login page: %d", code)
	}
	if code, _ := authPost(t, c, ts, "/api/auth/login", `{"password":"wrong"}`); code != http.StatusUnauthorized {
		t.Fatalf("bad login: %d", code)
	}
	if code, out := authPost(t, c, ts, "/api/auth/login", `{"password":"`+testPassword+`"}`); code != 200 {
		t.Fatalf("login: %d %v", code, out)
	}
	if code, _ := getStatus(t, c, ts.URL+"/api/config"); code != 200 {
		t.Fatalf("api after login: %d", code)
	}
	// 配置接口不返回密码
	resp, _ := c.Get(ts.URL + "/api/config")
	var cfg map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&cfg)
	resp.Body.Close()
	if a := fmtJSON(cfg["auth"]); strings.Contains(a, testPassword) || strings.Contains(a, "pbkdf2") {
		t.Fatalf("password leaked: %s", a)
	}
	// 通过 PUT /api/config 不能改掉密码
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/config", strings.NewReader(`{"auth":{"username":"","password":""}}`))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := c.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("put config: %v %v", resp, err)
	}
	if !s.Cfg.Get().Auth.HasPassword() {
		t.Fatal("password cleared via config PUT")
	}

	// 改密码：旧会话失效，当前浏览器拿到新会话
	other := newClient()
	authPost(t, other, ts, "/api/auth/login", `{"password":"`+testPassword+`"}`)
	if code, _ := authPost(t, c, ts, "/api/auth/password", `{"old_password":"x","password":"newpass1"}`); code != 400 {
		t.Fatalf("wrong old: %d", code)
	}
	if code, out := authPost(t, c, ts, "/api/auth/password", `{"old_password":"`+testPassword+`","password":"newpass1"}`); code != 200 {
		t.Fatalf("change: %d %v", code, out)
	}
	if code, _ := getStatus(t, c, ts.URL+"/api/config"); code != 200 {
		t.Fatalf("current session after change: %d", code)
	}
	if code, _ := getStatus(t, other, ts.URL+"/api/config"); code != http.StatusUnauthorized {
		t.Fatalf("other session should expire: %d", code)
	}
	if h := s.Cfg.Get().Auth; h.Password != "" || !strings.HasPrefix(h.PasswordHash, "pbkdf2") {
		t.Fatalf("stored: %+v", h)
	}

	// 退出登录
	authPost(t, c, ts, "/api/auth/logout", ``)
	if code, _ := getStatus(t, c, ts.URL+"/api/config"); code != http.StatusUnauthorized {
		t.Fatalf("after logout: %d", code)
	}
}

func TestLoginLockout(t *testing.T) {
	_, ts := newTestServer(t)
	c := newClient()
	for i := 0; i < maxFails; i++ {
		authPost(t, c, ts, "/api/auth/login", `{"password":"bad"}`)
	}
	if code, out := authPost(t, c, ts, "/api/auth/login", `{"password":"`+testPassword+`"}`); code != http.StatusTooManyRequests {
		t.Fatalf("expected lockout: %d %v", code, out)
	}
}

func TestFirstSetup(t *testing.T) {
	s, ts := newTestServer(t)
	cf := s.Cfg.Get()
	cf.Auth = config.Auth{}
	if err := s.Cfg.Update(cf); err != nil {
		t.Fatal(err)
	}
	c := newClient()
	resp, _ := c.Get(ts.URL + "/api/auth/state")
	var st map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st["password_set"] != false || st["can_setup"] != true {
		t.Fatalf("state: %v", st)
	}
	// 没设置密码时也不能直接访问
	if code, _ := getStatus(t, c, ts.URL+"/api/config"); code != http.StatusUnauthorized {
		t.Fatalf("api before setup: %d", code)
	}
	if code, _ := authPost(t, c, ts, "/api/auth/setup", `{"password":"123"}`); code != 400 {
		t.Fatalf("short password: %d", code)
	}
	if code, out := authPost(t, c, ts, "/api/auth/setup", `{"password":"first-pass"}`); code != 200 {
		t.Fatalf("setup: %d %v", code, out)
	}
	if code, _ := getStatus(t, c, ts.URL+"/api/config"); code != 200 {
		t.Fatalf("after setup: %d", code)
	}
	if code, _ := authPost(t, newClient(), ts, "/api/auth/setup", `{"password":"another1"}`); code != 400 {
		t.Fatalf("second setup must fail: %d", code)
	}
	// 会话签名不依赖进程内状态：换一个 Server 实例（模拟重启）仍然有效
	s2 := &Server{Cfg: s.Cfg, BaseDir: s.BaseDir}
	r, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/config", nil)
	for _, k := range c.Jar.Cookies(r.URL) {
		r.AddCookie(k)
	}
	if !s2.authorized(r, s.Cfg.Get().Auth) {
		t.Fatal("session should survive restart")
	}
}

func TestSetupRemoteRejected(t *testing.T) {
	s, _ := newTestServer(t)
	cf := s.Cfg.Get()
	cf.Auth = config.Auth{}
	_ = s.Cfg.Update(cf)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(`{"password":"remote-pass"}`))
	req.RemoteAddr = "192.168.1.50:5555"
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.withAuth(http.HandlerFunc(s.routeAPI)).ServeHTTP(w, req)
	if w.Code != http.StatusForbidden || s.Cfg.Get().Auth.HasPassword() {
		t.Fatalf("remote setup: %d %s", w.Code, w.Body.String())
	}
}

func TestSessionCookieAttrs(t *testing.T) {
	_, ts := newTestServer(t)
	resp, err := http.Post(ts.URL+"/api/auth/login", "application/json", strings.NewReader(`{"password":"`+testPassword+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	sc := resp.Header.Get("Set-Cookie")
	if !strings.Contains(sc, "Max-Age=2592000") || !strings.Contains(sc, "HttpOnly") || !strings.Contains(sc, "SameSite=Lax") {
		t.Fatalf("cookie: %s", sc)
	}
}

// 自动排布：不指定交换机时连未定位节点的坐标一起清除；指定交换机时只处理该交换机下的节点。
func TestLayoutResetIncludesUnlocated(t *testing.T) {
	s, ts := newTestServer(t)
	var unl, att string
	err := s.Topo.Update(func(tp *topology.Topology) error {
		for _, n := range tp.Nodes {
			if n.Attach.Switch == "" && unl == "" {
				unl = n.ID
				n.Position = &topology.Position{X: 10, Y: 20}
			} else if n.Attach.Switch != "" && att == "" {
				att = n.ID
				n.Position = &topology.Position{X: 30, Y: 40}
			}
		}
		return nil
	})
	if err != nil || unl == "" || att == "" {
		t.Fatalf("setup: %v %q %q", err, unl, att)
	}
	pos := func(id string) *topology.Position {
		tp, _ := s.Topo.Snapshot()
		for _, n := range tp.Nodes {
			if n.ID == id {
				return n.Position
			}
		}
		return nil
	}
	// 只处理另一台交换机：未定位节点不动
	tp, _ := s.Topo.Snapshot()
	other := ""
	for _, sw := range tp.Switches {
		other = sw.ID
	}
	c := newClient()
	authPost(t, c, ts, "/api/auth/login", `{"password":"`+testPassword+`"}`)
	if code, out := authPost(t, c, ts, "/api/layout/reset", `{"switch":"`+other+`"}`); code != 200 {
		t.Fatalf("reset switch: %d %v", code, out)
	}
	if pos(unl) == nil {
		t.Fatal("unlocated node reset by per-switch reset")
	}
	code, out := authPost(t, c, ts, "/api/layout/reset", `{}`)
	if code != 200 {
		t.Fatalf("reset all: %d %v", code, out)
	}
	if pos(unl) != nil || pos(att) != nil {
		t.Fatalf("positions not cleared: %v %v", pos(unl), pos(att))
	}
}
