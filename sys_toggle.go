// sys_toggle.go — DÉSACTIVATION RÉVERSIBLE d'un déclencheur système depuis l'Import.
// Best practice : on ne DÉTRUIT jamais. On rend inactif de façon réversible et visible :
//   - cron  : on commente la ligne avec un marqueur "#SB-OFF# " (réactivable en 1 clic).
//   - systemd: systemctl disable/enable --now (l'unité reste installée, seul l'état change).
//   - inotify (process vivant) : pas de désactivation réversible possible -> kill (SIGTERM),
//     clairement signalé comme NON réversible côté UI.
// Écriture cron : nécessite le dossier hôte monté en rw (voir compose "gère ton système").
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"sync"
	"time"
)

// marqueur de désactivation réversible (préfixe de ligne cron commentée par SyncBridge).
const sbOff = "#SB-OFF# "

func apiSystemToggle(w http.ResponseWriter, r *http.Request) {
	var it SysItem
	if err := json.NewDecoder(r.Body).Decode(&it); err != nil {
		http.Error(w, "requête invalide", http.StatusBadRequest)
		return
	}
	var state string
	var err error
	switch it.Type {
	case "cron":
		state, err = toggleCronLine(it)
	case "systemd-service", "systemd-timer", "systemd-path":
		state, err = toggleSystemdUnit(it.Name)
	case "inotify-proc":
		state, err = killInotify(it)
	default:
		err = fmt.Errorf("type %q non géré", it.Type)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"state": state})
}

// toggleCronLine : commente / décommente la ligne correspondante dans son fichier hôte.
// Réécriture atomique (fichier temp + rename). Réversible et lisible dans le fichier.
func toggleCronLine(it SysItem) (string, error) {
	data, err := os.ReadFile(it.File)
	if err != nil {
		return "", fmt.Errorf("lecture %s : %v", it.File, err)
	}
	lines := strings.Split(string(data), "\n")
	marker := strings.TrimSpace(sbOff) // "#SB-OFF#"
	newState, found := "", false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		off := strings.HasPrefix(trimmed, marker)
		body := trimmed
		if off {
			body = strings.TrimSpace(strings.TrimPrefix(trimmed, marker))
		}
		itp := parseCronLine(body, it.File, false)
		if itp == nil || itp.Schedule != it.Schedule || itp.Target != it.Target {
			continue
		}
		found = true
		if off {
			lines[i] = body // réactive
			newState = "enabled"
		} else {
			lines[i] = sbOff + line // désactive (garde la ligne, commentée)
			newState = "disabled"
		}
		break
	}
	if !found {
		return "", fmt.Errorf("ligne introuvable dans %s (déjà modifiée ?)", it.File)
	}
	tmp := it.File + ".sbtmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")), 0644); err != nil {
		return "", fmt.Errorf("écriture refusée sur %s : %v — le dossier hôte doit être monté en rw (voir compose)", it.File, err)
	}
	if err := os.Rename(tmp, it.File); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("remplacement %s : %v", it.File, err)
	}
	return newState, nil
}

// toggleSystemdUnit : bascule enable/disable --now selon l'état courant (is-enabled).
func toggleSystemdUnit(name string) (string, error) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return "", fmt.Errorf("systemctl indisponible dans le conteneur : gérer systemd demande un accès hôte privilégié (dbus). Tu peux gérer les cron ici, ou l'unité directement sur l'hôte")
	}
	en, _ := exec.Command("systemctl", "is-enabled", name).Output()
	if strings.TrimSpace(string(en)) == "enabled" {
		if out, err := exec.Command("systemctl", "disable", "--now", name).CombinedOutput(); err != nil {
			return "", fmt.Errorf("systemctl disable %s : %s", name, strings.TrimSpace(string(out)))
		}
		return "disabled", nil
	}
	if out, err := exec.Command("systemctl", "enable", "--now", name).CombinedOutput(); err != nil {
		return "", fmt.Errorf("systemctl enable %s : %s", name, strings.TrimSpace(string(out)))
	}
	return "enabled", nil
}

// killInotify : arrête un process inotifywait (SIGTERM). NON réversible (process vivant).
// Nécessite un espace PID partagé avec l'hôte (pid: host) pour cibler le bon process.
func killInotify(it SysItem) (string, error) {
	parts := strings.Split(it.File, "/")
	pid := ""
	for i, p := range parts {
		if p == "proc" && i+1 < len(parts) {
			pid = parts[i+1]
			break
		}
	}
	n, err := strconv.Atoi(pid)
	if err != nil {
		return "", fmt.Errorf("pid introuvable dans %s", it.File)
	}
	if err := syscall.Kill(n, syscall.SIGTERM); err != nil {
		return "", fmt.Errorf("kill %d : %v (nécessite pid:host partagé avec l'hôte)", n, err)
	}
	return "killed", nil
}

// ============================================================================
//  SUPPRESSION DÉFINITIVE + CORBEILLE (récupérable)
//  Best practice : jamais destructif "en douce". L'API exige le mot "delete",
//  accepte un LOT, et sauvegarde CHAQUE élément supprimé dans /config/sys-trash.json
//  (ligne cron d'origine ou contenu d'unité systemd) pour pouvoir le recréer.
// ============================================================================

type TrashEntry struct {
	TS         string `json:"ts"`
	Type       string `json:"type"`
	Name       string `json:"name"`
	File       string `json:"file"`
	Schedule   string `json:"schedule"`
	Target     string `json:"target"`
	Line       string `json:"line"` // ligne cron d'origine (restore = ré-ajout)
	Unit       string `json:"unit"` // contenu d'unité systemd d'origine
	Restorable bool   `json:"restorable"`
}

var trashMu sync.Mutex

func trashPath() string { return dataDir + "/sys-trash.json" }

func loadTrash() []TrashEntry {
	b, err := os.ReadFile(trashPath())
	if err != nil {
		return []TrashEntry{}
	}
	var t []TrashEntry
	if json.Unmarshal(b, &t) != nil || t == nil {
		return []TrashEntry{}
	}
	return t
}

func writeTrash(t []TrashEntry) {
	b, _ := json.MarshalIndent(t, "", "  ")
	os.WriteFile(trashPath(), b, 0600)
	own(trashPath())
}

func appendTrash(e TrashEntry) {
	trashMu.Lock()
	defer trashMu.Unlock()
	writeTrash(append([]TrashEntry{e}, loadTrash()...)) // plus récent en tête
}

func nowStamp() string { return time.Now().Format("2006-01-02 15:04:05") }

// apiSystemDelete : supprime un LOT de déclencheurs. Exige confirm=="delete".
func apiSystemDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Items   []SysItem `json:"items"`
		Confirm string    `json:"confirm"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		http.Error(w, "requête invalide", http.StatusBadRequest)
		return
	}
	if strings.ToLower(strings.TrimSpace(body.Confirm)) != "delete" {
		http.Error(w, `confirmation requise : tape "delete"`, http.StatusBadRequest)
		return
	}
	type Res struct {
		Name  string `json:"name"`
		State string `json:"state"`
		Error string `json:"error"`
	}
	out := []Res{}
	for _, it := range body.Items {
		var err error
		switch it.Type {
		case "cron":
			var line string
			if line, err = deleteCronLine(it); err == nil {
				appendTrash(TrashEntry{TS: nowStamp(), Type: it.Type, Name: it.Name, File: it.File,
					Schedule: it.Schedule, Target: it.Target, Line: line, Restorable: true})
			}
		case "systemd-service", "systemd-timer", "systemd-path":
			var content string
			if content, err = deleteSystemdUnit(it); err == nil {
				appendTrash(TrashEntry{TS: nowStamp(), Type: it.Type, Name: it.Name, File: it.File,
					Schedule: it.Schedule, Target: it.Target, Unit: content, Restorable: true})
			}
		case "inotify-proc":
			if _, err = killInotify(it); err == nil {
				appendTrash(TrashEntry{TS: nowStamp(), Type: it.Type, Name: it.Name, File: it.File,
					Target: it.Target, Restorable: false}) // process : non recréable à l'identique
			}
		default:
			err = fmt.Errorf("type %q non géré", it.Type)
		}
		if err != nil {
			out = append(out, Res{it.Name, "error", err.Error()})
		} else {
			out = append(out, Res{it.Name, "deleted", ""})
		}
	}
	writeJSON(w, out)
}

// deleteCronLine : retire la ligne correspondante du fichier hôte. Renvoie la ligne
// brute d'origine (pour la corbeille / restauration). Réécriture atomique.
func deleteCronLine(it SysItem) (string, error) {
	data, err := os.ReadFile(it.File)
	if err != nil {
		return "", fmt.Errorf("lecture %s : %v", it.File, err)
	}
	lines := strings.Split(string(data), "\n")
	marker := strings.TrimSpace(sbOff)
	removed, idx := "", -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		body := trimmed
		if strings.HasPrefix(trimmed, marker) {
			body = strings.TrimSpace(strings.TrimPrefix(trimmed, marker))
		}
		itp := parseCronLine(body, it.File, false)
		if itp == nil || itp.Schedule != it.Schedule || itp.Target != it.Target {
			continue
		}
		removed, idx = line, i
		break
	}
	if idx < 0 {
		return "", fmt.Errorf("ligne introuvable dans %s (déjà supprimée ?)", it.File)
	}
	lines = append(lines[:idx], lines[idx+1:]...)
	tmp := it.File + ".sbtmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")), 0644); err != nil {
		return "", fmt.Errorf("écriture refusée sur %s : %v — le dossier hôte doit être monté en rw", it.File, err)
	}
	if err := os.Rename(tmp, it.File); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("remplacement %s : %v", it.File, err)
	}
	return removed, nil
}

// deleteSystemdUnit : disable --now (best-effort) puis supprime le fichier d'unité.
// Renvoie le contenu original (corbeille / restauration).
func deleteSystemdUnit(it SysItem) (string, error) {
	content := ""
	if b, e := os.ReadFile(it.File); e == nil {
		content = string(b)
	}
	if _, err := exec.LookPath("systemctl"); err == nil {
		exec.Command("systemctl", "disable", "--now", it.Name).Run()
	}
	if err := os.Remove(it.File); err != nil {
		return content, fmt.Errorf("suppression %s : %v — le dossier systemd doit être monté en rw", it.File, err)
	}
	if _, err := exec.LookPath("systemctl"); err == nil {
		exec.Command("systemctl", "daemon-reload").Run()
	}
	return content, nil
}

// apiSystemTrash : liste la corbeille (éléments supprimés, récupérables).
func apiSystemTrash(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, loadTrash())
}

// apiSystemRestore : recrée un LOT d'éléments depuis la corbeille.
func apiSystemRestore(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Items []TrashEntry `json:"items"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		http.Error(w, "requête invalide", http.StatusBadRequest)
		return
	}
	type Res struct {
		Name  string `json:"name"`
		State string `json:"state"`
		Error string `json:"error"`
	}
	out := []Res{}
	for _, e := range body.Items {
		var err error
		switch {
		case e.Type == "cron":
			err = restoreCronLine(e)
		case strings.HasPrefix(e.Type, "systemd"):
			err = restoreSystemdUnit(e)
		default:
			err = fmt.Errorf("non restaurable (%s : process vivant)", e.Type)
		}
		if err == nil {
			removeFromTrash(e)
			out = append(out, Res{e.Name, "restored", ""})
		} else {
			out = append(out, Res{e.Name, "error", err.Error()})
		}
	}
	writeJSON(w, out)
}

func restoreCronLine(e TrashEntry) error {
	if e.Line == "" || e.File == "" {
		return fmt.Errorf("données insuffisantes pour restaurer")
	}
	data, _ := os.ReadFile(e.File)
	s := string(data)
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	s += e.Line + "\n"
	tmp := e.File + ".sbtmp"
	if err := os.WriteFile(tmp, []byte(s), 0644); err != nil {
		return fmt.Errorf("écriture %s : %v (rw requis)", e.File, err)
	}
	return os.Rename(tmp, e.File)
}

func restoreSystemdUnit(e TrashEntry) error {
	if e.Unit == "" || e.File == "" {
		return fmt.Errorf("contenu d'unité manquant")
	}
	if err := os.WriteFile(e.File, []byte(e.Unit), 0644); err != nil {
		return fmt.Errorf("écriture %s : %v (rw requis)", e.File, err)
	}
	if _, err := exec.LookPath("systemctl"); err == nil {
		exec.Command("systemctl", "daemon-reload").Run()
	}
	return nil
}

func removeFromTrash(e TrashEntry) {
	trashMu.Lock()
	defer trashMu.Unlock()
	var keep []TrashEntry
	for _, x := range loadTrash() {
		if x.TS == e.TS && x.File == e.File && x.Type == e.Type && x.Schedule == e.Schedule && x.Target == e.Target {
			continue
		}
		keep = append(keep, x)
	}
	writeTrash(keep)
}
