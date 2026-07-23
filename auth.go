// auth.go — authentification minimale, mono-utilisateur (homelab).
// 1er lancement : aucun compte -> l'UI propose de CRÉER un compte (register).
// Ensuite : login avec ces identifiants. Alternative : imposer le compte via les
// variables d'env SYNCBRIDGE_USER / SYNCBRIDGE_PASSWORD (prioritaires, pas de register).
// Mot de passe : hashé bcrypt dans /config/auth.json (jamais en clair).
// Sessions : token aléatoire (crypto/rand) en mémoire + cookie httpOnly.
// ponytail: sessions en RAM -> un redémarrage force un re-login ; largement suffisant
//   en mono-utilisateur. Passer à un cookie signé HMAC si un jour multi-instance.
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

type authFile struct {
	User string `json:"user"`
	Hash string `json:"hash"`
}

var (
	sessMu   sync.Mutex
	sessions = map[string]bool{}
)

func authPath() string { return filepath.Join(dataDir, "auth.json") }

// envCreds : identifiants imposés par variables d'env (prioritaires sur le fichier).
func envCreds() (string, string, bool) {
	u, p := os.Getenv("SYNCBRIDGE_USER"), os.Getenv("SYNCBRIDGE_PASSWORD")
	if u != "" && p != "" {
		return u, p, true
	}
	return "", "", false
}

// authConfigured : un compte existe-t-il (env OU fichier) ? Sinon -> écran register.
func authConfigured() bool {
	if _, _, ok := envCreds(); ok {
		return true
	}
	_, err := os.Stat(authPath())
	return err == nil
}

func loadAuthFile() (*authFile, bool) {
	data, err := os.ReadFile(authPath())
	if err != nil {
		return nil, false
	}
	var a authFile
	if json.Unmarshal(data, &a) != nil {
		return nil, false
	}
	return &a, true
}

// verifyCreds : env prioritaire (comparaison constante), sinon bcrypt du fichier.
func verifyCreds(user, pass string) bool {
	if u, p, ok := envCreds(); ok {
		return ctEq(user, u) && ctEq(pass, p)
	}
	a, ok := loadAuthFile()
	if !ok || !ctEq(user, a.User) {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(a.Hash), []byte(pass)) == nil
}

func ctEq(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func newSession(w http.ResponseWriter) {
	b := make([]byte, 32)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	sessMu.Lock()
	sessions[tok] = true
	sessMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "sb_session", Value: tok, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 86400 * 7})
}

func validSession(r *http.Request) bool {
	c, err := r.Cookie("sb_session")
	if err != nil {
		return false
	}
	sessMu.Lock()
	ok := sessions[c.Value]
	sessMu.Unlock()
	return ok
}

func clearSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("sb_session"); err == nil {
		sessMu.Lock()
		delete(sessions, c.Value)
		sessMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "sb_session", Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
}

// --- handlers ---

func apiAuthStatus(w http.ResponseWriter, r *http.Request) {
	_, _, env := envCreds()
	writeJSON(w, map[string]bool{
		"configured": authConfigured(),
		"authed":     validSession(r),
		"envManaged": env, // si true : compte fixé par env, register/changement désactivé
	})
}

func apiAuthRegister(w http.ResponseWriter, r *http.Request) {
	if authConfigured() {
		http.Error(w, "un compte est déjà configuré", http.StatusForbidden)
		return
	}
	var b struct{ User, Password string }
	json.NewDecoder(r.Body).Decode(&b)
	b.User = strings.TrimSpace(b.User)
	if len(b.User) < 3 || len(b.Password) < 6 {
		http.Error(w, "identifiant ≥ 3 caractères et mot de passe ≥ 6 caractères", http.StatusBadRequest)
		return
	}
	h, err := bcrypt.GenerateFromPassword([]byte(b.Password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "erreur de hachage", http.StatusInternalServerError)
		return
	}
	data, _ := json.MarshalIndent(authFile{User: b.User, Hash: string(h)}, "", "  ")
	if err := os.WriteFile(authPath(), data, 0600); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	own(authPath())
	newSession(w)
	writeJSON(w, map[string]bool{"ok": true})
}

func apiAuthLogin(w http.ResponseWriter, r *http.Request) {
	var b struct{ User, Password string }
	json.NewDecoder(r.Body).Decode(&b)
	if !verifyCreds(strings.TrimSpace(b.User), b.Password) {
		http.Error(w, "identifiants invalides", http.StatusUnauthorized)
		return
	}
	newSession(w)
	writeJSON(w, map[string]bool{"ok": true})
}

func apiAuthLogout(w http.ResponseWriter, r *http.Request) {
	clearSession(w, r)
	writeJSON(w, map[string]bool{"ok": true})
}

// requireAuth : middleware global. Laisse passer /api/auth/* ; exige une session pour
// tout le reste. Non authentifié : 401 sur /api/*, sinon sert l'écran d'auth (page).
func requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/api/auth/") || validSession(r) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(p, "/api/") {
			http.Error(w, "non authentifié", http.StatusUnauthorized)
			return
		}
		data, _ := webFS.ReadFile("web/auth.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})
}
