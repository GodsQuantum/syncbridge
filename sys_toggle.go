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
