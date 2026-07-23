// system_backend.go — backend "System" : matérialise un job dans l'HÔTE (cron.d ou
// unité systemd) au lieu de l'exécuter dans SyncBridge. Avantage : le job SURVIT à
// une panne Docker/SyncBridge (c'est le système qui l'exécute). Contrepartie : pas
// de logs live/kill (on relit le système). Écriture = accès rw aux dossiers hôte
// montés (+ systemd nécessite un conteneur privilégié / socket dbus).
//
// Invariant "rien d'invisible / pas de doublon" : on retire TOUJOURS l'artefact
// avant de le réécrire, et basculer de backend nettoie l'autre côté (voir schedule()).
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// dossiers d'écriture hôte (montés rw). Par défaut = mêmes chemins que la lecture M4.
func cronWriteDir() string    { return env("SB_SYS_CRON_WRITE", "/host/etc/cron.d") }
func systemdWriteDir() string { return env("SB_SYS_SYSTEMD_WRITE", "/host/etc/systemd/system") }

func unitBase(id int) string    { return fmt.Sprintf("syncbridge-%d", id) }
func cronFilePath(id int) string { return filepath.Join(cronWriteDir(), unitBase(id)) }

// writeSystemArtifact : matérialise le job dans le système hôte.
func writeSystemArtifact(j *Job) error {
	// commande hôte : pour un job commande, la commande brute (le cron/sh l'exécute) ;
	// pour un job sync, la vraie ligne rsync/rclone. On n'enveloppe PAS dans sh -c
	// (sinon le quoting saute pour les commandes multi-mots).
	var cmd string
	if j.Kind == "command" {
		cmd = j.Command
	} else {
		cmd = strings.Join(buildCmd(j, false), " ")
	}
	switch j.Trigger {
	case "cron":
		return writeCronD(j, cmd)
	case "watch":
		return writeSystemdPath(j, cmd)
	default:
		return fmt.Errorf("backend System : trigger %q non supporté (cron ou watch requis)", j.Trigger)
	}
}

// writeCronD : écrit /etc/cron.d/syncbridge-<id> (exécuté par le cron de l'hôte).
// flock -n empêche le chevauchement ; PATH/SHELL fixés (piège classique du cron).
func writeCronD(j *Job, cmd string) error {
	if err := os.MkdirAll(cronWriteDir(), 0755); err != nil {
		return err
	}
	lock := fmt.Sprintf("/tmp/%s.lock", unitBase(j.ID))
	content := fmt.Sprintf(
		"%s job %d: %s (managed — ne pas editer a la main)\n"+
			"SHELL=/bin/sh\n"+
			"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n"+
			"%s root flock -n %s %s\n",
		sbMarker, j.ID, j.Name, j.Cron, lock, cmd)
	return os.WriteFile(cronFilePath(j.ID), []byte(content), 0644)
}

// writeSystemdPath : écrit un couple .service + .path (inotify natif de systemd),
// puis daemon-reload + enable --now. Le .path relance le .service à chaque écriture.
func writeSystemdPath(j *Job, cmd string) error {
	dir := systemdWriteDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	base := unitBase(j.ID)
	svc := fmt.Sprintf("[Unit]\nDescription=%s job %d: %s (managed)\n\n[Service]\nType=oneshot\nExecStart=/bin/sh -c %q\n",
		sbMarker, j.ID, j.Name, cmd)
	pth := fmt.Sprintf("[Unit]\nDescription=%s watch %d: %s (managed)\n\n[Path]\nPathModified=%s\nUnit=%s.service\n\n[Install]\nWantedBy=multi-user.target\n",
		sbMarker, j.ID, j.Name, strings.TrimRight(j.Source, "/"), base)
	if err := os.WriteFile(filepath.Join(dir, base+".service"), []byte(svc), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, base+".path"), []byte(pth), 0644); err != nil {
		return err
	}
	systemctl("daemon-reload")
	systemctl("enable", "--now", base+".path")
	return nil
}

// removeSystemArtifact : retire tout artefact hôte de ce job (idempotent).
func removeSystemArtifact(j *Job) {
	os.Remove(cronFilePath(j.ID))
	base := unitBase(j.ID)
	dir := systemdWriteDir()
	if _, err := os.Stat(filepath.Join(dir, base+".path")); err == nil {
		systemctl("disable", "--now", base+".path")
		os.Remove(filepath.Join(dir, base+".path"))
		os.Remove(filepath.Join(dir, base+".service"))
		systemctl("daemon-reload")
	}
}

// systemctl : best-effort. Si indisponible (conteneur non privilégié), on le signale
// clairement plutôt que d'échouer en silence. Les fichiers d'unité restent écrits.
func systemctl(args ...string) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		fmt.Printf("[system] systemctl indisponible : backend System/systemd requiert un accès hôte privilégié (dbus). Les unités sont écrites mais pas activées.\n")
		return
	}
	if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
		fmt.Printf("[system] systemctl %s : %v — %s\n", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
}
