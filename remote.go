// remote.go — pilotage multi-instances (façon Dockge).
// Une instance SyncBridge peut piloter d'AUTRES instances SyncBridge du réseau
// local : on enregistre {nom, url, identifiants} et on PROXIFIE les appels API vers
// l'instance distante. L'UI bascule d'instance ; tout le reste (jobs, import système,
// corbeille, logs live) fonctionne tel quel à travers le proxy — donc depuis HAL on
// pilote réellement les crons/jobs de SAL, sans monter ses volumes.
//
// Pourquoi un proxy et pas du SSH : on ne peut pas monter les volumes système d'un
// autre serveur ; en revanche chaque instance sait déjà lire/gérer SON propre système.
// On parle donc à son SyncBridge, pas à ses disques.
//
// Sécurité : seul l'admin authentifié localement peut ajouter/piloter un remote
// (requireAuth couvre /api/remote*). Le proxy s'authentifie au remote avec ses
// identifiants stockés et cache le cookie de session (re-login auto si expiré).
// ponytail: mot de passe du remote stocké en clair dans /config/remotes.json (0600) —
// même modèle de confiance que Dockge et que jobs.json (volume /config de confiance).
// Chiffrer si un jour /config n'est plus sur une machine sûre.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Remote struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"` // ex http://192.168.1.50:8788
	User     string `json:"user"`
	Password string `json:"password"` // requis pour s'authentifier au remote
}

var (
	remotesMu    sync.Mutex         // sérialise les écritures de remotes.json
	remoteCkMu   sync.Mutex         // protège le cache de cookies
	remoteCookie = map[int]string{} // cache du cookie de session par remote
)

func remotesPath() string { return dataDir + "/remotes.json" }

func loadRemotes() []Remote {
	b, err := os.ReadFile(remotesPath())
	if err != nil {
		return []Remote{}
	}
	var rs []Remote
	if json.Unmarshal(b, &rs) != nil || rs == nil {
		return []Remote{}
	}
	return rs
}

func saveRemotes(rs []Remote) {
	b, _ := json.MarshalIndent(rs, "", "  ")
	os.WriteFile(remotesPath(), b, 0600)
	own(remotesPath())
}

func getRemote(id int) *Remote {
	for _, r := range loadRemotes() {
		if r.ID == id {
			rr := r
			return &rr
		}
	}
	return nil
}

// ---- CRUD ----

// apiRemotes : GET liste (SANS mots de passe) | POST ajoute (avec test de connexion).
func apiRemotes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		type safe struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
			URL  string `json:"url"`
			User string `json:"user"`
		}
		out := []safe{}
		for _, rm := range loadRemotes() {
			out = append(out, safe{rm.ID, rm.Name, rm.URL, rm.User})
		}
		writeJSON(w, out)
	case "POST":
		var rm Remote
		if json.NewDecoder(r.Body).Decode(&rm) != nil || strings.TrimSpace(rm.Name) == "" || strings.TrimSpace(rm.URL) == "" {
			http.Error(w, "nom et url requis", http.StatusBadRequest)
			return
		}
		rm.Name = strings.TrimSpace(rm.Name)
		rm.URL = strings.TrimRight(strings.TrimSpace(rm.URL), "/")
		if !strings.HasPrefix(rm.URL, "http://") && !strings.HasPrefix(rm.URL, "https://") {
			rm.URL = "http://" + rm.URL
		}
		// test de connexion immédiat : évite d'enregistrer une instance injoignable / mauvais mdp.
		if _, err := remoteLogin(&rm); err != nil {
			http.Error(w, "connexion au remote échouée : "+err.Error(), http.StatusBadGateway)
			return
		}
		remotesMu.Lock()
		rs := loadRemotes()
		id := 1
		for _, x := range rs {
			if x.ID >= id {
				id = x.ID + 1
			}
		}
		rm.ID = id
		saveRemotes(append(rs, rm))
		remotesMu.Unlock()
		writeJSON(w, map[string]any{"id": rm.ID, "name": rm.Name})
	default:
		http.Error(w, "méthode", http.StatusMethodNotAllowed)
	}
}

// apiRemoteByID : DELETE /api/remotes/{id}.
func apiRemoteByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != "DELETE" {
		http.Error(w, "méthode", http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/remotes/"))
	if err != nil {
		http.Error(w, "id", http.StatusBadRequest)
		return
	}
	remotesMu.Lock()
	keep := []Remote{}
	for _, x := range loadRemotes() {
		if x.ID != id {
			keep = append(keep, x)
		}
	}
	saveRemotes(keep)
	remotesMu.Unlock()
	remoteCkMu.Lock()
	delete(remoteCookie, id)
	remoteCkMu.Unlock()
	writeJSON(w, map[string]int{"deleted": id})
}

// ---- authentification au remote ----

// remoteLogin : POST /api/auth/login sur l'instance distante, renvoie le cookie de session.
func remoteLogin(rm *Remote) (string, error) {
	body, _ := json.Marshal(map[string]string{"User": rm.User, "Password": rm.Password})
	cl := &http.Client{Timeout: 8 * time.Second}
	resp, err := cl.Post(rm.URL+"/api/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("injoignable (%v)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("identifiants refusés (code %d)", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "sb_session" {
			return c.Value, nil
		}
	}
	return "", fmt.Errorf("pas de cookie de session renvoyé (est-ce bien une instance SyncBridge ?)")
}

// remoteCookieFor : cookie en cache, sinon login. Le proxy invalide le cache sur 401.
func remoteCookieFor(rm *Remote) (string, error) {
	remoteCkMu.Lock()
	ck := remoteCookie[rm.ID]
	remoteCkMu.Unlock()
	if ck != "" {
		return ck, nil
	}
	ck, err := remoteLogin(rm)
	if err != nil {
		return "", err
	}
	remoteCkMu.Lock()
	remoteCookie[rm.ID] = ck
	remoteCkMu.Unlock()
	return ck, nil
}

// ---- proxy ----

// apiRemoteProxy : /api/remote/{id}/api/... -> {remote.URL}/api/... avec le cookie du remote.
// httputil.ReverseProxy gère nativement le streaming SSE (logs live) et le flush.
func apiRemoteProxy(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/remote/")
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		http.Error(w, "chemin invalide", http.StatusBadRequest)
		return
	}
	id, err := strconv.Atoi(rest[:slash])
	if err != nil {
		http.Error(w, "id", http.StatusBadRequest)
		return
	}
	rm := getRemote(id)
	if rm == nil {
		http.Error(w, "remote introuvable", http.StatusNotFound)
		return
	}
	target, err := url.Parse(rm.URL)
	if err != nil {
		http.Error(w, "url remote invalide", http.StatusInternalServerError)
		return
	}
	ck, err := remoteCookieFor(rm)
	if err != nil {
		http.Error(w, "auth remote : "+err.Error(), http.StatusBadGateway)
		return
	}
	upstreamPath := rest[slash:] // "/api/..."

	rp := httputil.NewSingleHostReverseProxy(target)
	rp.Director = func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = upstreamPath // RawQuery conservée telle quelle
		req.Host = target.Host
		req.Header.Set("Cookie", "sb_session="+ck)
	}
	rp.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode == http.StatusUnauthorized {
			// cookie expiré côté remote : on l'oublie -> re-login au prochain appel.
			remoteCkMu.Lock()
			delete(remoteCookie, id)
			remoteCkMu.Unlock()
		}
		return nil
	}
	rp.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		http.Error(w, "remote injoignable : "+err.Error(), http.StatusBadGateway)
	}
	rp.ServeHTTP(w, r)
}
