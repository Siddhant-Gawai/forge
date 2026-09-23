package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type authAttempt struct {
	Count int
	Until time.Time
}

func (a *App) allowAuth(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.authAttempts == nil {
		a.authAttempts = map[string]authAttempt{}
	}
	for key, entry := range a.authAttempts {
		if time.Now().After(entry.Until) {
			delete(a.authAttempts, key)
		}
	}
	entry := a.authAttempts[host]
	if entry.Until.IsZero() {
		entry.Until = time.Now().Add(15 * time.Minute)
	}
	entry.Count++
	a.authAttempts[host] = entry
	return entry.Count <= 30
}
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
func (a *App) startSession(w http.ResponseWriter, u string) error {
	token := id() + id()
	expires := time.Now().Add(12 * time.Hour)
	if a.store.pg != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := a.store.pg.db.ExecContext(ctx, "INSERT INTO forge.sessions(token_hash,user_id,expires_at) VALUES($1,$2,$3)", tokenHash(token), u, expires); err != nil {
			return safeDatabaseError(err)
		}
		_, _ = a.store.pg.db.ExecContext(ctx, "DELETE FROM forge.sessions WHERE expires_at < now()")
	} else {
		a.mu.Lock()
		a.sessions[token] = session{u, expires}
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "forge_session", Value: token, Path: "/", HttpOnly: true, Secure: strings.HasPrefix(a.baseURL, "https:"), SameSite: http.SameSiteLaxMode, MaxAge: 43200})
	return nil
}
func (a *App) signup(w http.ResponseWriter, r *http.Request) {
	if !a.allowAuth(r) {
		fail(w, 429, "too many attempts; try again in 15 minutes")
		return
	}
	var v struct {
		Name     string `json:"name"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(w, r, &v); err != nil {
		fail(w, 400, err.Error())
		return
	}
	v.Name = strings.TrimSpace(v.Name)
	v.Email = strings.ToLower(strings.TrimSpace(v.Email))
	email, err := mail.ParseAddress(v.Email)
	if !nonempty(v.Name) || len(v.Name) > 100 || err != nil || email.Address != v.Email || len(v.Email) > 254 {
		fail(w, 400, "enter your name and a valid email address")
		return
	}
	if len(v.Password) < 12 || len(v.Password) > 72 {
		fail(w, 400, "password must contain 12–72 bytes")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(v.Password), bcrypt.DefaultCost)
	if err != nil {
		fail(w, 500, "could not create account")
		return
	}
	uid := id()
	err = a.transaction(func(s *State) error {
		for _, account := range s.Accounts {
			if account.Email == v.Email {
				return reject(409, "an account with this email already exists")
			}
		}
		s.Users = append(s.Users, User{ID: uid, Name: v.Name})
		s.Accounts = append(s.Accounts, Account{ID: uid, Email: v.Email, PasswordHash: string(hash), CreatedAt: now()})
		o := Organization{ID: id(), Name: v.Name + "’s workspace", Members: map[string]string{uid: "owner"}}
		s.Organizations = append(s.Organizations, o)
		record(s, "", o.ID, uid, "account.signup", "Personal workspace created", true)
		return nil
	})
	if err != nil {
		actionError(w, err)
		return
	}
	if err = a.startSession(w, uid); err != nil {
		fail(w, 503, "account created; please log in to start a session")
		return
	}
	jsonResponse(w, 201, map[string]string{"user_id": uid})
}

var dummyPasswordHash = func() []byte {
	h, _ := bcrypt.GenerateFromPassword([]byte("not-an-account-password"), bcrypt.DefaultCost)
	return h
}()

func (a *App) passwordLogin(email, password string) string {
	if len(password) > 72 {
		return ""
	}
	email = strings.ToLower(strings.TrimSpace(email))
	a.store.mu.Lock()
	uid := ""
	hash := dummyPasswordHash
	for _, account := range a.store.state.Accounts {
		if account.Email == email {
			uid = account.ID
			hash = []byte(account.PasswordHash)
			break
		}
	}
	a.store.mu.Unlock()
	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil {
		return ""
	}
	return uid
}
