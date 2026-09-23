package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSignupLoginIsolationAndLogout(t *testing.T) {
	a := testApp(t)
	a.demo = false
	payload := map[string]string{"name": "New Builder", "email": "Builder@Example.com", "password": "a-long-password-123"}
	w := request(a, "POST", "/api/signup", "", payload)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	var signed map[string]string
	json.Unmarshal(w.Body.Bytes(), &signed)
	uid := signed["user_id"]
	cookie := w.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatal("cookie protection missing")
	}
	if strings.Contains(a.store.state.Accounts[0].PasswordHash, payload["password"]) {
		t.Fatal("password stored in plaintext")
	}
	if a.store.state.Accounts[0].Email != "builder@example.com" {
		t.Fatal("email not normalized")
	}
	r := httptest.NewRequest("GET", "/api/state", nil)
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), "orbit-api") || strings.Contains(w.Body.String(), "password_hash") || strings.Contains(w.Body.String(), "Alex Morgan") {
		t.Fatal("new account sees another tenant or secrets", w.Body)
	}
	orgs := a.store.state.Organizations
	own := orgs[len(orgs)-1]
	if own.Members[uid] != "owner" {
		t.Fatal("workspace owner missing")
	}
	duplicate := request(a, "POST", "/api/signup", "", payload)
	if duplicate.Code != 409 {
		t.Fatal(duplicate.Code)
	}
	bad := request(a, "POST", "/api/login", "", map[string]string{"email": payload["email"], "password": "wrong"})
	if bad.Code != 401 {
		t.Fatal(bad.Code)
	}
	good := request(a, "POST", "/api/login", "", map[string]string{"email": "BUILDER@example.com", "password": payload["password"]})
	if good.Code != 200 {
		t.Fatal(good.Body)
	}
	r = httptest.NewRequest("POST", "/api/logout", strings.NewReader("{}"))
	r.Header.Set("X-Requested-With", "forge")
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	r = httptest.NewRequest("GET", "/api/state", nil)
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("logout failed")
	}
	reopened, err := openStore(a.store.path)
	if err != nil {
		t.Fatal(err)
	}
	app2 := newApp(reopened, false, "http://localhost")
	defer app2.cancel()
	good = request(app2, "POST", "/api/login", "", map[string]string{"email": payload["email"], "password": payload["password"]})
	if good.Code != 200 {
		t.Fatal("account did not persist")
	}
}
func TestSignupValidationAndRateLimit(t *testing.T) {
	a := testApp(t)
	for _, v := range []map[string]string{{"name": "Person", "email": "bad", "password": "long-password-123"}, {"name": "Person", "email": "a@example.com", "password": "short"}, {"name": "", "email": "a@example.com", "password": "long-password-123"}} {
		if w := request(a, "POST", "/api/signup", "", v); w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
	for i := 0; i < 30; i++ {
		request(a, "POST", "/api/login", "", map[string]string{"email": "none@example.com", "password": "wrong"})
	}
	if w := request(a, "POST", "/api/login", "", map[string]string{"email": "none@example.com", "password": "wrong"}); w.Code != 429 {
		t.Fatal("rate limit missing")
	}
}
