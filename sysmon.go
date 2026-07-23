// sysmon.go — lecture SEULE de tout ce qui déclenche des scripts sur l'hôte.
// Objectif "rien ne m'échappe" : crontabs, unités systemd (service/timer/path),
// et watchers inotifywait tournant en fond. Aucun privilège requis : on lit des
// fichiers montés en RO + /proc. L'état actif/inactif fin (systemctl) viendra
// quand le socket dbus sera monté ; ici on lit les définitions installées.
package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// SysItem = un déclencheur système détecté.
type SysItem struct {
	Type     string `json:"type"`     // cron | systemd-service | systemd-timer | systemd-path | inotify-proc
	Name     string `json:"name"`     // nom lisible (unité, fichier, ou pid)
	Schedule string `json:"schedule"` // expr cron / OnCalendar / chemin surveillé
	Target   string `json:"target"`   // commande / ExecStart / script
	File     string `json:"file"`     // fichier source
	Managed  bool   `json:"managed"`  // true si posé par SyncBridge (marqueur)
}

// marqueur inséré par SyncBridge dans les artefacts qu'il écrit (backend=system).
const sbMarker = "# syncbridge"

func sysCronPaths() []string {
	if v := os.Getenv("SB_SYS_CRON_PATHS"); v != "" {
		return strings.Split(v, ":")
	}
	return []string{"/host/etc/crontab", "/host/etc/cron.d", "/host/crontabs"}
}

func sysSystemdPaths() []string {
	if v := os.Getenv("SB_SYS_SYSTEMD_PATHS"); v != "" {
		return strings.Split(v, ":")
	}
	return []string{"/host/etc/systemd/system"}
}

// apiSystemScan : renvoie tous les déclencheurs système détectés.
func apiSystemScan(w http.ResponseWriter, r *http.Request) {
	items := []SysItem{}
	items = append(items, scanCron()...)
	items = append(items, scanSystemd()...)
	items = append(items, scanInotifyProcs()...)
	writeJSON(w, map[string]any{
		"items":        items,
		"cronPaths":    sysCronPaths(),
		"systemdPaths": sysSystemdPaths(),
	})
}

// --- CRON : /etc/crontab, /etc/cron.d/*, crontabs utilisateur ---
func scanCron() []SysItem {
	var out []SysItem
	for _, p := range sysCronPaths() {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		files := []string{p}
		if fi.IsDir() {
			files, _ = filepath.Glob(filepath.Join(p, "*"))
		}
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			managed := strings.Contains(string(data), sbMarker)
			for _, line := range strings.Split(string(data), "\n") {
				if it := parseCronLine(line, f, managed); it != nil {
					out = append(out, *it)
				}
			}
		}
	}
	return out
}

// parseCronLine : extrait planning + commande d'une ligne cron.
// Gère /etc/crontab & cron.d (champ user en 6e position) ET crontab user (pas de user).
func parseCronLine(line, file string, managed bool) *SysItem {
	l := strings.TrimSpace(line)
	if l == "" || strings.HasPrefix(l, "#") || strings.Contains(l, "=") && !strings.HasPrefix(l, "*") && !isCronField(strings.Fields(l)[0]) {
		return nil // commentaire ou variable d'env (PATH=..., SHELL=...)
	}
	f := strings.Fields(l)
	if len(f) < 6 {
		return nil
	}
	// les 5 premiers champs doivent être des champs cron
	for i := 0; i < 5; i++ {
		if !isCronField(f[i]) {
			return nil
		}
	}
	sched := strings.Join(f[:5], " ")
	rest := f[5:]
	// cron.d / /etc/crontab : 6e champ = user si ce n'est pas un chemin/commande évident
	inCronD := strings.Contains(file, "cron.d") || strings.HasSuffix(file, "crontab")
	if inCronD && len(rest) > 1 && !strings.ContainsAny(rest[0], "/.") {
		rest = rest[1:] // saute le user
	}
	return &SysItem{Type: "cron", Name: filepath.Base(file), Schedule: sched,
		Target: strings.Join(rest, " "), File: file, Managed: managed}
}

// --- SYSTEMD : lit les .service/.timer/.path installés ---
func scanSystemd() []SysItem {
	var out []SysItem
	for _, dir := range sysSystemdPaths() {
		for _, ext := range []string{"*.service", "*.timer", "*.path"} {
			files, _ := filepath.Glob(filepath.Join(dir, ext))
			for _, f := range files {
				data, err := os.ReadFile(f)
				if err != nil {
					continue
				}
				s := string(data)
				it := SysItem{Name: filepath.Base(f), File: f, Managed: strings.Contains(s, sbMarker)}
				switch {
				case strings.HasSuffix(f, ".service"):
					it.Type = "systemd-service"
					it.Target = iniVal(s, "ExecStart")
				case strings.HasSuffix(f, ".timer"):
					it.Type = "systemd-timer"
					it.Schedule = iniVal(s, "OnCalendar")
				case strings.HasSuffix(f, ".path"):
					it.Type = "systemd-path"
					it.Schedule = firstNonEmpty(iniVal(s, "PathModified"), iniVal(s, "PathChanged"), iniVal(s, "PathExists"))
				}
				out = append(out, it)
			}
		}
	}
	return out
}

// iniVal : 1re valeur d'une clé "Key=..." dans un fichier unit (best-effort).
func iniVal(content, key string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, key+"=") {
			return strings.TrimSpace(strings.TrimPrefix(line, key+"="))
		}
	}
	return ""
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// --- INOTIFY : repère les inotifywait lancés en fond (surveillances hors SyncBridge) ---
// Nécessite le /proc de l'hôte monté (sinon voit seulement le conteneur).
func scanInotifyProcs() []SysItem {
	var out []SysItem
	procs, _ := filepath.Glob("/host/proc/[0-9]*/cmdline")
	if len(procs) == 0 {
		procs, _ = filepath.Glob("/proc/[0-9]*/cmdline")
	}
	for _, cf := range procs {
		data, err := os.ReadFile(cf)
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
		if len(args) == 0 || !strings.Contains(args[0], "inotifywait") {
			continue
		}
		pid := filepath.Base(filepath.Dir(cf))
		out = append(out, SysItem{Type: "inotify-proc", Name: "pid " + pid,
			Schedule: "inotifywait", Target: strings.Join(args, " "), File: cf})
	}
	return out
}
