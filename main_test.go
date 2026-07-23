package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Filtre motif + signature de dossier (cœur du watcher NFS-safe).
func TestWatchGlobAndSig(t *testing.T) {
	match := func(globs []string) func(string) bool {
		return func(name string) bool {
			if len(globs) == 0 {
				return true
			}
			for _, g := range globs {
				if ok, _ := filepath.Match(g, filepath.Base(name)); ok {
					return true
				}
			}
			return false
		}
	}
	mp4 := match([]string{"*.mp4"})
	if !mp4("/x/clip.mp4") || mp4("/x/note.md") {
		t.Fatal("filtre *.mp4 incorrect")
	}

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.mp4"), []byte("aa"), 0644)
	s1 := dirSig(dir, mp4)
	os.WriteFile(filepath.Join(dir, "b.md"), []byte("ignored"), 0644)
	if dirSig(dir, mp4) != s1 {
		t.Fatal("un .md filtré ne devrait pas changer la signature")
	}
	os.WriteFile(filepath.Join(dir, "c.mp4"), []byte("cc"), 0644)
	if dirSig(dir, mp4) == s1 {
		t.Fatal("un nouveau .mp4 devrait changer la signature")
	}
	if dirSig("/nfs/monte/perdu/zzz", mp4) != "" {
		t.Fatal("dossier absent doit renvoyer une signature vide (montage perdu)")
	}
}

// Parseur cron système : gère user crontab, /etc/cron.d (champ user), commentaires.
func TestParseCronLine(t *testing.T) {
	// user crontab (pas de champ user)
	it := parseCronLine("0 3 * * * /home/user/scripts/cleanup.sh", "/host/crontabs/user", false)
	if it == nil || it.Schedule != "0 3 * * *" || !strings.Contains(it.Target, "cleanup.sh") {
		t.Fatalf("user crontab mal parsé: %+v", it)
	}
	// /etc/cron.d (6e champ = user root, à sauter)
	it = parseCronLine("0 4 * * * root /opt/backup.sh", "/host/etc/cron.d/backup", false)
	if it == nil || it.Target != "/opt/backup.sh" {
		t.Fatalf("cron.d user non sauté: %+v", it)
	}
	// commentaire et variable d'env ignorés
	if parseCronLine("# commentaire", "/x", false) != nil {
		t.Fatal("commentaire non ignoré")
	}
	if parseCronLine("PATH=/usr/bin:/bin", "/x", false) != nil {
		t.Fatal("variable d'env non ignorée")
	}
}

// Garde anti-catastrophe rsync : détecter source vide / démontée.
func TestDirIsEmptyOrMissing(t *testing.T) {
	if !dirIsEmptyOrMissing("/n/existe/pas/du/tout") {
		t.Fatal("dossier absent (montage perdu) doit être détecté comme vide/manquant")
	}
	empty := t.TempDir()
	if !dirIsEmptyOrMissing(empty) {
		t.Fatal("dossier vide doit être détecté")
	}
	full := t.TempDir()
	os.WriteFile(filepath.Join(full, "f"), []byte("x"), 0644)
	if dirIsEmptyOrMissing(full) {
		t.Fatal("dossier non vide ne doit PAS être signalé")
	}
}

// Backend System : la ligne cron.d doit porter le marqueur, flock (anti-overlap)
// et PATH (piège classique du cron), et la commande brute sans double sh -c.
func TestSystemBackendCronD(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SB_SYS_CRON_WRITE", dir)
	j := &Job{ID: 7, Name: "prune", Kind: "command", Command: "docker system prune -f", Trigger: "cron", Cron: "0 4 * * *"}
	if err := writeSystemArtifact(j); err != nil {
		t.Fatalf("écriture cron.d: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "syncbridge-7"))
	s := string(b)
	for _, want := range []string{sbMarker, "flock -n", "PATH=", "0 4 * * * root", "docker system prune -f"} {
		if !strings.Contains(s, want) {
			t.Fatalf("cron.d ne contient pas %q :\n%s", want, s)
		}
	}
	if strings.Contains(s, "sh -c docker") {
		t.Fatal("la commande ne doit pas être double-enveloppée dans sh -c")
	}
	// removeSystemArtifact doit nettoyer
	removeSystemArtifact(j)
	if _, err := os.Stat(filepath.Join(dir, "syncbridge-7")); err == nil {
		t.Fatal("artefact non supprimé")
	}
}

// Vérifie que le branchement Kind route bien vers le bon moteur.
func TestBuildCmdKind(t *testing.T) {
	// Job commande -> sh -c "<command>"
	c := buildCmd(&Job{Kind: "command", Command: "docker system prune -f"}, false)
	if len(c) != 3 || c[0] != "sh" || c[1] != "-c" || c[2] != "docker system prune -f" {
		t.Fatalf("command: attendu [sh -c ...], obtenu %v", c)
	}

	// Job sync (Kind vide = rétrocompat) -> rsync
	c = buildCmd(&Job{Source: "/a", Dest: "/b", Engine: "rsync", Mode: "mirror"}, false)
	if c[0] != "rsync" {
		t.Fatalf("sync: attendu rsync, obtenu %v", c[0])
	}

	// validateJob : commande vide refusée, cron 5 champs exigé
	if validateJob(&Job{Kind: "command", Command: ""}) == "" {
		t.Fatal("commande vide aurait dû être refusée")
	}
	if validateJob(&Job{Kind: "command", Command: "ls", Trigger: "cron", Cron: "bad"}) == "" {
		t.Fatal("cron invalide aurait dû être refusé")
	}
	if validateJob(&Job{Kind: "command", Command: "ls", Trigger: "manual"}) != "" {
		t.Fatal("job commande manuel valide refusé à tort")
	}
}
