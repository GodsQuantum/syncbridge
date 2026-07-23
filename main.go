// SyncBridge — sync local-to-local. Moteur rsync/rclone. Binaire unique Go.
package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/robfig/cron/v3"
)

//go:embed web/*
var webFS embed.FS

var (
	dataDir = env("SB_DATA", "/config")
	jobsMu  sync.Mutex
	jobs    = map[int]*Job{}
	nextID  = 1
	runs    = map[int]*Run{} // état live par job
	runsMu  sync.Mutex
	sched   = cron.New(cron.WithSeconds())
	cronIDs = map[int]cron.EntryID{}
	watchMu    sync.Mutex
	watches    = map[int]*fsnotify.Watcher{}
	watchStops = map[int]chan struct{}{} // signal d'arrêt des goroutines watch (event + poll)
)

// Job = une tâche de sync.
type Job struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`    // sync (défaut, rétrocompat) | command
	Command    string `json:"command"` // si kind=command : commande/script shell à exécuter
	Backend    string `json:"backend"` // syncbridge (défaut) | system : exécuté par l'hôte (cron.d/systemd)
	Source     string `json:"source"`
	Dest       string `json:"dest"`
	Engine     string `json:"engine"`     // rsync | rclone
	Mode       string `json:"mode"`       // add | mirror | move
	Trigger    string `json:"trigger"`    // manual | cron | watch
	Cron       string `json:"cron"`       // si trigger=cron (5 champs)
	WatchGlob  string `json:"watchGlob"`  // si watch : filtre motif "*.mp4,*.m4a" (vide = tout)
	WatchMode  string `json:"watchMode"`  // si watch : event | poll | hybrid (défaut hybrid, NFS-safe)
	Debounce   int    `json:"debounce"`   // si watch : anti-rebond en s (défaut 10)
	PollSec    int    `json:"pollSec"`    // si watch (poll/hybrid) : intervalle de scan en s (défaut 300)
	Timeout    int    `json:"timeout"`    // délai max d'exécution en s (0 = illimité) : tue un job qui pend
	Compare    string `json:"compare"`    // time | checksum
	Bwlimit    string `json:"bwlimit"`    // ex "10M", "" = illimité
	Backup     bool   `json:"backup"`     // fichiers supprimés/écrasés -> dossier daté
	BackupKeep int    `json:"backupKeep"` // nb de dossiers .sb-backup à garder (0 = tous)
	MaxDel     int    `json:"maxDel"`     // 0 = pas de limite ; abandonne si > N suppressions
	SkipNew    bool   `json:"skipNew"`    // ne pas écraser fichier + récent à destination
	SysBackup  bool   `json:"sysBackup"`  // backup système fidèle : -aHAX --numeric-ids --fake-super
	Exclude    string `json:"exclude"`    // motifs séparés par virgule
	Disabled   bool   `json:"disabled"`   // si true : pas de cron/watch, pas de run auto
	LastRun    string `json:"lastRun"`
	LastStat   string `json:"lastStat"` // ok | error | running
}

// Run = état d'exécution en cours (logs live).
type Run struct {
	mu       sync.Mutex
	Lines    []string
	Running  bool
	RC       int
	Started  time.Time // début du run
	Progress string    // dernière ligne de progression rsync (%, débit, ETA)
	cmd      *exec.Cmd // process en cours (pour kill)
	killed   bool      // arrêté manuellement
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// UID/GID des fichiers créés par l'app (défaut 1000:1000).
// Même si le conteneur tourne en root, ses fichiers de config restent en 1000.
var (
	fileUID = atoiDef(env("SB_UID", "1000"), 1000)
	fileGID = atoiDef(env("SB_GID", "1000"), 1000)
)

func atoiDef(s string, d int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return d
}

// own force le propriétaire d'un fichier/dossier à SB_UID:SB_GID (best-effort).
func own(path string) { _ = os.Chown(path, fileUID, fileGID) }

// ---------- persistance JSON ----------
func jobsFile() string { return filepath.Join(dataDir, "jobs.json") }

func save() {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	list := make([]*Job, 0, len(jobs))
	for _, j := range jobs {
		list = append(list, j)
	}
	sort.Slice(list, func(a, b int) bool { return list[a].ID < list[b].ID })
	b, err := json.MarshalIndent(struct {
		Next int    `json:"next"`
		Jobs []*Job `json:"jobs"`
	}{nextID, list}, "", "  ")
	if err != nil {
		fmt.Printf("[save] erreur marshal: %v\n", err)
		return
	}
	// écriture atomique : temp + rename, pour ne jamais corrompre jobs.json
	// (si le process meurt en plein write, l'ancien fichier reste intact).
	tmp := jobsFile() + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		fmt.Printf("[save] erreur écriture: %v\n", err)
		return
	}
	own(tmp)
	if err := os.Rename(tmp, jobsFile()); err != nil {
		fmt.Printf("[save] erreur rename: %v\n", err)
		return
	}
	own(jobsFile()) // garde jobs.json en 1000 même si conteneur root
}

func load() {
	b, err := os.ReadFile(jobsFile())
	if err != nil {
		return
	}
	var d struct {
		Next int    `json:"next"`
		Jobs []*Job `json:"jobs"`
	}
	if json.Unmarshal(b, &d) != nil {
		return
	}
	nextID = d.Next
	for _, j := range d.Jobs {
		// un job marqué "running" dans le fichier = run interrompu par un reboot.
		// On le repasse à un état neutre pour ne pas le bloquer à vie.
		if j.LastStat == "running" {
			j.LastStat = "interrompu"
		}
		jobs[j.ID] = j
	}
}

// ---------- construction commande moteur ----------
func buildCmd(j *Job, dry bool) []string {
	if j.Kind == "command" {
		// Job commande : exécute une commande/script shell arbitraire (scripts CRON,
		// docker prune, export Paperless, utilitaires...). Le dry-run n'a pas de sens
		// pour une commande libre : on le laisse à l'UI de ne pas le proposer.
		return []string{"sh", "-c", j.Command}
	}
	src := strings.TrimRight(j.Source, "/") + "/"
	dst := strings.TrimRight(j.Dest, "/") + "/"

	if j.Engine == "rclone" {
		verb := "copy"
		if j.Mode == "mirror" {
			verb = "sync"
		} else if j.Mode == "move" {
			verb = "move"
		}
		c := []string{"rclone", verb, src, dst, "--progress", "--stats=1s", "--stats-one-line"}
		if j.Compare == "checksum" {
			c = append(c, "--checksum")
		}
		if j.Bwlimit != "" {
			c = append(c, "--bwlimit", j.Bwlimit)
		}
		if j.Backup {
			c = append(c, "--backup-dir", filepath.Join(dst, ".sb-backup", time.Now().Format("20060102-150405")))
			c = append(c, "--exclude", ".sb-backup/**")
		}
		if j.MaxDel > 0 {
			c = append(c, "--max-delete", strconv.Itoa(j.MaxDel))
		}
		if j.SkipNew {
			c = append(c, "--update")
		}
		for _, e := range splitCSV(j.Exclude) {
			c = append(c, "--exclude", e)
		}
		if dry {
			c = append(c, "--dry-run")
		}
		return c
	}

	// rsync
	c := []string{"rsync", "-a", "--info=progress2,stats2", "--human-readable"}
	if j.SysBackup {
		// Backup système fidèle : préserve propriétaires/perms/ACL/xattr/hardlinks.
		// --fake-super : stocke la vraie identité dans les xattr (le fichier reste
		// physiquement 1000 sur le NFS squashé, mais restaurable à l'identique).
		c = append(c, "-HAX", "--numeric-ids", "--fake-super")
	} else {
		// Comportement normal : tout appartient à 1000:1000 partout (même disque local),
		// pas de préservation des propriétaires root.
		c = append(c, "--chown=1000:1000")
	}
	if j.Mode == "mirror" {
		c = append(c, "--delete")
	}
	if j.Mode == "move" {
		c = append(c, "--remove-source-files")
	}
	if j.Compare == "checksum" {
		c = append(c, "--checksum")
	}
	if j.Bwlimit != "" {
		c = append(c, "--bwlimit", strings.TrimSuffix(j.Bwlimit, "B"))
	}
	if j.Backup {
		c = append(c, "--backup", "--backup-dir",
			filepath.Join(dst, ".sb-backup", time.Now().Format("20060102-150405")))
		c = append(c, "--exclude", ".sb-backup") // ne pas syncer/supprimer la corbeille elle-même
	}
	if j.MaxDel > 0 {
		c = append(c, "--max-delete", strconv.Itoa(j.MaxDel))
	}
	if j.SkipNew {
		c = append(c, "--update")
	}
	for _, e := range splitCSV(j.Exclude) {
		c = append(c, "--exclude", e)
	}
	if dry {
		c = append(c, "--dry-run", "--itemize-changes")
	}
	c = append(c, src, dst)
	return c
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// humanSummary : explique en français ce que le job va faire, avant la commande.
func humanSummary(j *Job, dry bool) string {
	if j.Kind == "command" {
		return "• Commande : " + j.Command + "\n"
	}
	var b strings.Builder
	modeTxt := map[string]string{
		"add":    "AJOUTE les nouveaux fichiers et met à jour les modifiés (aucune suppression dans la destination)",
		"mirror": "REND la destination identique à la source (les fichiers absents de la source seront SUPPRIMÉS dans la destination)",
		"move":   "COPIE vers la destination puis VIDE la source",
	}[j.Mode]
	b.WriteString("• Action : " + modeTxt + "\n")
	b.WriteString("• De  : " + j.Source + "\n")
	b.WriteString("• Vers: " + j.Dest + "\n")
	if j.SysBackup {
		b.WriteString("• BACKUP SYSTÈME FIDÈLE : propriétaires/permissions/ACL/hardlinks préservés via xattr (restaurable à l'identique)\n")
	}
	if j.Compare == "checksum" {
		b.WriteString("• Comparaison par checksum (lecture complète du contenu)\n")
	}
	if j.Backup {
		keep := "toutes les versions gardées"
		if j.BackupKeep > 0 {
			keep = fmt.Sprintf("%d dernières versions gardées", j.BackupKeep)
		}
		b.WriteString("• Corbeille de sécurité active (" + keep + ")\n")
	}
	if j.MaxDel > 0 {
		b.WriteString(fmt.Sprintf("• Garde-fou : abandon si plus de %d suppressions\n", j.MaxDel))
	}
	if dry {
		b.WriteString("• SIMULATION : rien n'est modifié. La liste ci-dessous montre ce qui CHANGERAIT.\n")
		b.WriteString("  (colonnes rsync : > = à envoyer, c = créé, d = dossier, * = supprimé)\n")
	}
	return b.String()
}

// rotateBackups : ne garde que les N dossiers .sb-backup les plus récents.
func rotateBackups(dst string, keep int) {
	if keep <= 0 {
		return
	}
	base := filepath.Join(dst, ".sb-backup")
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) <= keep {
		return
	}
	sort.Strings(dirs) // noms horodatés YYYYMMDD-HHMMSS -> tri chrono
	for _, old := range dirs[:len(dirs)-keep] {
		os.RemoveAll(filepath.Join(base, old))
		fmt.Printf("[backup] purge ancienne version: %s\n", old)
	}
}

// abortRun : enregistre un run avorté VISIBLE dans le dashboard (logs + statut error),
// plutôt qu'un refus silencieux. Respecte le mot d'ordre : rien d'invisible.
func abortRun(id int, msg string) {
	stamp := time.Now().Format("2006-01-02 15:04:05")
	r := &Run{Lines: []string{fmt.Sprintf("=== ABANDON [%s] ===\n%s\n", stamp, msg)}, RC: 1}
	runsMu.Lock()
	runs[id] = r
	runsMu.Unlock()
	setStat(id, "error", stamp)
	fmt.Printf("[abort] job %d : %s\n", id, msg)
}

// dirIsEmptyOrMissing : true si le dossier est absent, illisible (montage perdu),
// ou ne contient aucune entrée. Sert de garde anti-catastrophe avant un --delete.
func dirIsEmptyOrMissing(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return true // absent ou inaccessible
	}
	defer f.Close()
	names, err := f.Readdirnames(1) // lit au plus 1 entrée : suffit pour "vide ?"
	if err != nil {
		return true
	}
	return len(names) == 0
}

// ---------- exécution ----------
func execJob(id int, dry bool) {
	jobsMu.Lock()
	j := jobs[id]
	jobsMu.Unlock()
	if j == nil {
		return
	}

	// GARDES DE SÉCURITÉ (jobs sync uniquement).
	if j.Kind != "command" {
		cleanSrc := strings.TrimRight(j.Source, "/")
		cleanDst := strings.TrimRight(j.Dest, "/")
		// 1) source == destination : risque de destruction.
		if cleanSrc == cleanDst && cleanSrc != "" {
			abortRun(id, "source et destination identiques : run refusé (risque de perte de données)")
			return
		}
		// 2) ANTI-CATASTROPHE rsync : en miroir/déplacement, une source VIDE ou
		// inaccessible (montage NFS perdu) effacerait TOUTE la destination via
		// --delete. C'est LE grand classique de perte de données. On abandonne.
		if j.Mode == "mirror" || j.Mode == "move" {
			if dirIsEmptyOrMissing(j.Source) {
				abortRun(id, "source vide ou inaccessible (montage NFS perdu ?) : run ABANDONNÉ pour ne pas effacer la destination. Vérifie que "+j.Source+" est bien monté et non vide.")
				return
			}
		}
	}

	// VERROU anti-chevauchement : un job ne lance jamais un nouveau run réel
	// si le précédent tourne encore. Empêche l'empilement (watch rapide, cron
	// qui retombe avant la fin, etc.). Le dry-run n'est pas verrouillé.
	if !dry {
		runsMu.Lock()
		if runs[id] != nil && runs[id].Running {
			runsMu.Unlock()
			fmt.Printf("[skip] job %d déjà en cours, déclenchement ignoré\n", id)
			return
		}
		// réserve tout de suite le slot (état running) pour bloquer les suivants
		runs[id] = &Run{Lines: []string{}, Running: true, RC: -1, Started: time.Now()}
		runsMu.Unlock()
	}

	cmdArgs := buildCmd(j, dry)
	stamp := time.Now().Format("2006-01-02 15:04:05")
	label := ternary(dry, "SIMULATION", "SYNC")
	if j.Kind == "command" {
		label = "COMMANDE"
	}
	head := fmt.Sprintf("=== %s [%s] %s ===\n%s$ %s\n",
		label, stamp, j.Name, humanSummary(j, dry), strings.Join(cmdArgs, " "))

	r := &Run{Lines: []string{head}, Running: true, RC: -1, Started: time.Now()}
	fmt.Print(head)
	runsMu.Lock()
	runs[id] = r
	runsMu.Unlock()

	if !dry {
		setStat(id, "running", "")
	}

	// Timeout : un job qui pend indéfiniment est pire qu'un job qui échoue.
	ctx := context.Background()
	if j.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(j.Timeout)*time.Second)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, cmdArgs[0], cmdArgs[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // groupe de process -> kill récursif
	// au timeout/annulation, tue tout le groupe (rsync + enfants), pas juste le parent
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			if pgid, e := syscall.Getpgid(cmd.Process.Pid); e == nil {
				return syscall.Kill(-pgid, syscall.SIGKILL)
			}
			return cmd.Process.Kill()
		}
		return nil
	}
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	rc := 0
	if err := cmd.Start(); err != nil {
		r.append(fmt.Sprintf("ERREUR: %v\n", err))
		rc = 127
	} else {
		r.mu.Lock()
		r.cmd = cmd
		r.mu.Unlock()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		// Split sur \n ET \r : rsync --info=progress2 met à jour la ligne de
		// progression avec des \r (retour chariot). Sans ça, le % n'apparaît
		// jamais tant que le transfert n'est pas fini.
		sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
			if atEOF && len(data) == 0 {
				return 0, nil, nil
			}
			for i, b := range data {
				if b == '\n' || b == '\r' {
					return i + 1, data[:i], nil
				}
			}
			if atEOF {
				return len(data), data, nil
			}
			return 0, nil, nil // demande plus de données
		})
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line != "" {
				r.append(line + "\n")
			}
		}
		if err := cmd.Wait(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				rc = ee.ExitCode()
			} else {
				rc = 1
			}
		}
		if ctx.Err() == context.DeadlineExceeded {
			r.append(fmt.Sprintf("\n⏱ TIMEOUT : job tué après %d s (limite atteinte)\n", j.Timeout))
		}
	}

	r.mu.Lock()
	r.Running = false
	r.RC = rc
	wasKilled := r.killed
	if wasKilled {
		r.Lines = append(r.Lines, "\n=== arrêté ===\n")
	} else {
		r.Lines = append(r.Lines, fmt.Sprintf("\n=== terminé (code %d) ===\n", rc))
	}
	r.mu.Unlock()

	if !dry {
		st := "ok"
		if wasKilled {
			st = "arrêté"
		} else if rc != 0 {
			st = "error"
		}
		setStat(id, st, stamp)
		if !wasKilled && rc == 0 && j.Backup && j.BackupKeep > 0 {
			rotateBackups(strings.TrimRight(j.Dest, "/"), j.BackupKeep)
		}
	}
}

// killJob tue le run en cours d'un job (process + tous ses enfants).
// Renvoie true si un run était actif.
func killJob(id int) bool {
	runsMu.Lock()
	run := runs[id]
	runsMu.Unlock()
	if run == nil {
		return false
	}
	run.mu.Lock()
	running := run.Running
	cmd := run.cmd
	run.killed = true
	run.mu.Unlock()
	if !running || cmd == nil || cmd.Process == nil {
		return false
	}
	// tue tout le groupe de process (rsync + éventuels enfants)
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err == nil {
		syscall.Kill(-pgid, syscall.SIGKILL)
	} else {
		cmd.Process.Kill()
	}
	fmt.Printf("[kill] job %d arrêté manuellement\n", id)
	run.append(fmt.Sprintf("\n=== ARRÊTÉ MANUELLEMENT (job %d) ===\n", id))
	return true
}

func (r *Run) append(l string) {	r.mu.Lock()
	r.Lines = append(r.Lines, l)
	// capture la dernière ligne de progression rsync (contient un %)
	if strings.Contains(l, "%") {
		r.Progress = strings.TrimSpace(l)
	}
	r.mu.Unlock()
	fmt.Print(l)
}

func setStat(id int, stat, when string) {
	jobsMu.Lock()
	if j := jobs[id]; j != nil {
		j.LastStat = stat
		if when != "" {
			j.LastRun = when
		}
	}
	jobsMu.Unlock()
	save()
}

func ternary(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

// ---------- planification / watch ----------
func schedule(j *Job) {
	// retire ancien cron
	if id, ok := cronIDs[j.ID]; ok {
		sched.Remove(id)
		delete(cronIDs, j.ID)
	}
	// retire ancien watch
	stopWatch(j.ID)

	// job désactivé : rien ne tourne, ni en-process ni sur l'hôte.
	if j.Disabled {
		removeSystemArtifact(j)
		return
	}

	// BACKEND SYSTEM : le job est matérialisé dans l'hôte (cron.d/systemd), pas
	// exécuté en-process. On (ré)écrit l'artefact (écrasement atomique = pas de doublon).
	if j.Backend == "system" {
		if err := writeSystemArtifact(j); err != nil {
			fmt.Printf("[system] job %d : %v\n", j.ID, err)
		} else {
			fmt.Printf("[system] job %d matérialisé sur l'hôte (%s)\n", j.ID, j.Trigger)
		}
		return
	}

	// BACKEND SYNCBRIDGE : on s'assure qu'aucun artefact hôte résiduel ne double
	// le job (ex. après bascule de backend), puis on enregistre en-process.
	removeSystemArtifact(j)

	switch j.Trigger {
	case "cron":
		if j.Cron == "" {
			return
		}
		// robfig attend 6 champs (avec secondes) ; on préfixe "0 " pour du cron 5 champs classique
		spec := j.Cron
		if len(strings.Fields(spec)) == 5 {
			spec = "0 " + spec
		}
		if id, err := sched.AddFunc(spec, func() {
			fmt.Printf("[cron] déclenchement job %d (%s)\n", j.ID, j.Name)
			execJob(j.ID, false)
		}); err == nil {
			cronIDs[j.ID] = id
			fmt.Printf("[sched] job %d planifié: %s\n", j.ID, j.Cron)
		} else {
			fmt.Printf("[sched] job %d cron invalide: %s (%v)\n", j.ID, j.Cron, err)
		}
	case "watch":
		startWatch(j.ID, j.Source)
	}
}

// startWatch : déclenche le job quand le dossier surveillé change.
// Robuste NFS : mode hybride = fsnotify (rapide, changements LOCAUX) + polling
// (indispensable car inotify ne voit PAS les écritures distantes sur NFS/SMB).
// La boucle survit à une perte de montage (le scan renvoie "" et attend le retour).
func startWatch(id int, dir string) {
	jobsMu.Lock()
	j := jobs[id]
	jobsMu.Unlock()
	if j == nil {
		return
	}
	mode := j.WatchMode
	if mode == "" {
		mode = "hybrid"
	}
	deb := j.Debounce
	if deb <= 0 {
		deb = 10
	}
	poll := j.PollSec
	if poll <= 0 {
		poll = 300
	}
	globs := splitCSV(j.WatchGlob)

	stop := make(chan struct{})
	watchMu.Lock()
	watchStops[id] = stop
	watchMu.Unlock()

	// match : le fichier passe-t-il le filtre motif ? (vide = tout accepté)
	match := func(name string) bool {
		if len(globs) == 0 {
			return true
		}
		base := filepath.Base(name)
		for _, g := range globs {
			if ok, _ := filepath.Match(strings.TrimSpace(g), base); ok {
				return true
			}
		}
		return false
	}

	// déclencheur debouncé partagé : les events fsnotify ET le polling y convergent.
	// Le timer se ré-arme à chaque signal -> ne tire qu'après un silence de `deb`
	// secondes = fin d'écriture au niveau du dossier (les events ont cessé).
	var tmu sync.Mutex
	var timer *time.Timer
	var fire func(string) // pré-déclaré : le timer se ré-arme lui-même (récursion)
	fire = func(reason string) {
		tmu.Lock()
		defer tmu.Unlock()
		if timer != nil {
			timer.Stop()
		}
		timer = time.AfterFunc(time.Duration(deb)*time.Second, func() {
			// VÉRIF FIN D'ÉCRITURE : ne lance que si le dossier est stable ~2 s
			// (aucun fichier en cours de copie). Sinon on re-arme et on réessaie.
			if !stableFor(dir, match, 2*time.Second) {
				fmt.Printf("[watch] job %d : écriture en cours, déclenchement repoussé\n", id)
				fire("écriture terminée")
				return
			}
			fmt.Printf("[watch] job %d : déclenchement (%s)\n", id, reason)
			execJob(id, false)
		})
	}

	// --- fsnotify : chemin rapide pour les changements locaux ---
	if mode == "event" || mode == "hybrid" {
		if w, err := fsnotify.NewWatcher(); err == nil {
			addRecursive(w, dir)
			watchMu.Lock()
			watches[id] = w
			watchMu.Unlock()
			go func() {
				for {
					select {
					case ev, ok := <-w.Events:
						if !ok {
							return
						}
						if ev.Op&fsnotify.Create != 0 {
							if fi, e := os.Stat(ev.Name); e == nil && fi.IsDir() {
								w.Add(ev.Name) // nouveau sous-dossier surveillé à la volée
							}
						}
						if match(ev.Name) {
							fire("event")
						}
					case _, ok := <-w.Errors:
						if !ok {
							return
						}
						// erreur watcher (ex: montage perdu) : on NE quitte PAS la boucle.
					case <-stop:
						return
					}
				}
			}()
		}
	}

	// --- polling : filet de sécurité NFS (voit les changements distants) ---
	if mode == "poll" || mode == "hybrid" {
		go func() {
			last := dirSig(dir, match)
			t := time.NewTicker(time.Duration(poll) * time.Second)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					sig := dirSig(dir, match)
					// "" = dossier illisible (montage perdu) : on ignore ce tour.
					if sig != "" && sig != last {
						last = sig
						fire("polling")
					}
				case <-stop:
					return
				}
			}
		}()
	}
}

// dirSig : signature bon marché d'un dossier (nb fichiers + taille totale + mtime max),
// filtrée par `match`. Renvoie "" si le dossier est inaccessible (NFS démonté) pour que
// le polling saute le tour sans fausse alerte ni crash.
func dirSig(dir string, match func(string) bool) string {
	if _, err := os.Stat(dir); err != nil {
		return "" // montage perdu / dossier absent
	}
	var n, size, maxMod int64
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !match(p) {
			return nil
		}
		if fi, e := d.Info(); e == nil {
			n++
			size += fi.Size()
			if m := fi.ModTime().Unix(); m > maxMod {
				maxMod = m
			}
		}
		return nil
	})
	return fmt.Sprintf("%d:%d:%d", n, size, maxMod)
}

// stableFor : true si la signature du dossier ne bouge pas pendant `d`
// (= plus aucune écriture en cours). Renvoie false si inaccessible (montage perdu),
// auquel cas le polling reprendra le relais.
func stableFor(dir string, match func(string) bool, d time.Duration) bool {
	a := dirSig(dir, match)
	if a == "" {
		return false
	}
	time.Sleep(d)
	return dirSig(dir, match) == a
}

func addRecursive(w *fsnotify.Watcher, root string) {
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			// si w.Add échoue (limite fs.inotify.max_user_watches atteinte),
			// on le signale : le mode polling continue de couvrir le dossier.
			if e := w.Add(p); e != nil {
				fmt.Printf("[watch] ⚠ surveillance impossible sur %s (%v) — le polling prend le relais\n", p, e)
			}
		}
		return nil
	})
}

func stopWatch(id int) {
	watchMu.Lock()
	if w, ok := watches[id]; ok {
		w.Close()
		delete(watches, id)
	}
	if s, ok := watchStops[id]; ok {
		close(s) // arrête les goroutines event + poll
		delete(watchStops, id)
	}
	watchMu.Unlock()
}

func reloadSchedules() {
	jobsMu.Lock()
	list := make([]*Job, 0, len(jobs))
	for _, j := range jobs {
		list = append(list, j)
	}
	jobsMu.Unlock()
	for _, j := range list {
		schedule(j)
	}
}

// ---------- HTTP ----------
func main() {
	os.MkdirAll(dataDir, 0755)
	own(dataDir) // dossier config en 1000 même si conteneur root
	load()
	sched.Start()
	reloadSchedules()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/jobs", apiJobs)
	mux.HandleFunc("/api/jobs/", apiJobByID)
	mux.HandleFunc("/api/browse", apiBrowse)
	mux.HandleFunc("/api/engines", apiEngines)
	mux.HandleFunc("/api/status", apiStatus)
	mux.HandleFunc("/api/import/scan", apiImportScan)
	mux.HandleFunc("/api/system/scan", apiSystemScan)
	mux.HandleFunc("/api/system/toggle", apiSystemToggle)
	mux.HandleFunc("/api/system/delete", apiSystemDelete)
	mux.HandleFunc("/api/system/trash", apiSystemTrash)
	mux.HandleFunc("/api/system/restore", apiSystemRestore)
	mux.HandleFunc("/api/auth/status", apiAuthStatus)
	mux.HandleFunc("/api/auth/register", apiAuthRegister)
	mux.HandleFunc("/api/auth/login", apiAuthLogin)
	mux.HandleFunc("/api/auth/logout", apiAuthLogout)

	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(sub)))

	addr := ":8787" // port interne FIXE ; l'utilisateur mappe via "ports:" dans le compose
	fmt.Printf("SyncBridge démarré | port interne 8787 | data %s | rsync=%v rclone=%v | %d job(s)\n",
		dataDir, hasBin("rsync"), hasBin("rclone"), len(jobs))
	checkRsyncXattr()
	http.ListenAndServe(addr, requireAuth(mux))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func apiEngines(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]bool{
		"rsync":  hasBin("rsync"),
		"rclone": hasBin("rclone"),
	})
}

func hasBin(n string) bool { _, err := exec.LookPath(n); return err == nil }

// ---------- IMPORT : scan des scripts/crontab pour trouver rsync/rclone ----------

// Found = une commande de sync détectée dans un fichier.
type Found struct {
	Engine  string `json:"engine"`  // rsync | rclone
	Verb    string `json:"verb"`    // sync | copy | move (rclone) ou "" (rsync)
	Source  string `json:"source"`  // 1er chemin
	Dest    string `json:"dest"`    // 2e chemin
	Cron    string `json:"cron"`    // planning si trouvé dans une crontab
	File    string `json:"file"`    // fichier source de la ligne
	Line    string `json:"line"`    // ligne brute (tronquée)
	Local   bool   `json:"local"`   // true si les 2 chemins sont locaux (importable direct)
	Warning string `json:"warning"` // remarque (remote cloud, options complexes...)
}

// chemins scannés (montés en RO dans le conteneur). Configurable.
func importPaths() []string {
	if v := os.Getenv("SB_IMPORT_PATHS"); v != "" {
		return strings.Split(v, ":")
	}
	// défauts raisonnables (dossiers montés en lecture seule dans le conteneur)
	return []string{"/import/scripts", "/import/crontab"}
}

// parseSyncLine extrait une commande rsync/rclone d'une ligne de texte.
func parseSyncLine(line, file string) *Found {
	l := strings.TrimSpace(line)
	if l == "" || strings.HasPrefix(l, "#") {
		return nil
	}
	// détecte le cron éventuel en tête (5 champs) — crontab
	cronExpr := ""
	fields := strings.Fields(l)
	if len(fields) >= 6 && isCronField(fields[0]) && isCronField(fields[1]) {
		cronExpr = strings.Join(fields[:5], " ")
	}

	var f *Found
	if idx := strings.Index(l, "rclone "); idx >= 0 {
		f = parseRclone(l[idx:])
	} else if idx := strings.Index(l, "rsync "); idx >= 0 {
		f = parseRsync(l[idx:])
	}
	if f == nil {
		return nil
	}
	f.Cron = cronExpr
	f.File = file
	if len(l) > 200 {
		f.Line = l[:200] + "…"
	} else {
		f.Line = l
	}
	// local si aucun chemin ne ressemble à un remote (contient ":")
	f.Local = !looksRemote(f.Source) && !looksRemote(f.Dest)
	if !f.Local {
		f.Warning = "contient un remote (cloud/SSH) — vérifier avant import"
	}
	return f
}

func parseRclone(s string) *Found {
	toks := tokenize(s)
	// toks[0]=rclone toks[1]=verb ; cherche 2 args non-option après le verbe
	if len(toks) < 2 {
		return nil
	}
	verb := toks[1]
	if verb != "sync" && verb != "copy" && verb != "move" && verb != "copyto" && verb != "moveto" {
		return nil // bisync, mount, etc. : pas un simple A->B
	}
	args := nonOptionArgs(toks[2:])
	if len(args) < 2 {
		return nil
	}
	return &Found{Engine: "rclone", Verb: verb, Source: args[0], Dest: args[1]}
}

func parseRsync(s string) *Found {
	toks := tokenize(s)
	args := nonOptionArgs(toks[1:]) // saute "rsync"
	if len(args) < 2 {
		return nil
	}
	// rsync peut avoir >2 chemins (plusieurs sources) : on prend le 1er et le dernier (dest)
	return &Found{Engine: "rsync", Source: args[0], Dest: args[len(args)-1]}
}

// tokenize : découpe en respectant les guillemets simples/doubles.
func tokenize(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t':
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// nonOptionArgs : garde les tokens qui ne commencent pas par '-' et ne sont pas
// des valeurs d'option connues. Approche simple : ignore tout ce qui commence par '-'.
func nonOptionArgs(toks []string) []string {
	var out []string
	for _, t := range toks {
		if strings.HasPrefix(t, "-") {
			continue
		}
		// coupe une redirection ou pipe éventuelle
		if t == "|" || t == ">" || t == ">>" || t == "&&" || t == ";" {
			break
		}
		out = append(out, t)
	}
	return out
}

func looksRemote(p string) bool {
	// remote rclone "name:" ou SSH "user@host:" — un ':' hors chemin windows
	if strings.Contains(p, "@") && strings.Contains(p, ":") {
		return true
	}
	if i := strings.Index(p, ":"); i > 0 && !strings.HasPrefix(p, "/") {
		return true
	}
	return false
}

func isCronField(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r == '*' || r == '/' || r == ',' || r == '-') {
			return false
		}
	}
	return s != ""
}

// apiImportScan : scanne les chemins montés et renvoie les commandes rsync/rclone trouvées.
func apiImportScan(w http.ResponseWriter, r *http.Request) {
	found := []Found{}
	seen := map[string]bool{}
	for _, root := range importPaths() {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			// évite les binaires/gros fichiers
			if fi, e := d.Info(); e == nil && fi.Size() > 512*1024 {
				return nil
			}
			data, e := os.ReadFile(p)
			if e != nil {
				return nil
			}
			for _, line := range strings.Split(string(data), "\n") {
				if f := parseSyncLine(line, p); f != nil {
					key := f.Engine + f.Source + f.Dest
					if !seen[key] {
						seen[key] = true
						found = append(found, *f)
					}
				}
			}
			return nil
		})
	}
	writeJSON(w, map[string]any{"found": found, "paths": importPaths()})
}


// apiStatus : état live de chaque job pour l'UI (running, durée, progression, prochain run).
func apiStatus(w http.ResponseWriter, r *http.Request) {
	type St struct {
		ID       int    `json:"id"`
		Running  bool   `json:"running"`
		Since    int64  `json:"since"`    // secondes écoulées depuis le début du run
		Progress string `json:"progress"` // dernière ligne de progression rsync
		NextRun  string `json:"nextRun"`  // prochaine exécution cron (ISO), "" si aucune
	}
	out := []St{}
	jobsMu.Lock()
	ids := make([]int, 0, len(jobs))
	for id := range jobs {
		ids = append(ids, id)
	}
	jobsMu.Unlock()
	sort.Ints(ids)
	for _, id := range ids {
		s := St{ID: id}
		runsMu.Lock()
		if run := runs[id]; run != nil {
			run.mu.Lock()
			s.Running = run.Running
			if run.Running && !run.Started.IsZero() {
				s.Since = int64(time.Since(run.Started).Seconds())
			}
			s.Progress = run.Progress
			run.mu.Unlock()
		}
		runsMu.Unlock()
		// prochain run cron
		if cid, ok := cronIDs[id]; ok {
			e := sched.Entry(cid)
			if !e.Next.IsZero() {
				s.NextRun = e.Next.Format(time.RFC3339)
			}
		}
		out = append(out, s)
	}
	writeJSON(w, out)
}

// checkRsyncXattr : vérifie que rsync gère les xattr (requis pour --fake-super).
// Log un avertissement clair si absent, plutôt que d'échouer silencieusement.
func checkRsyncXattr() {
	if !hasBin("rsync") {
		return
	}
	out, _ := exec.Command("rsync", "--version").Output()
	s := string(out)
	if strings.Contains(s, "xattrs") || strings.Contains(s, "ACLs") {
		fmt.Println("[check] rsync supporte ACL/xattr : backup système fidèle OK")
	} else {
		fmt.Println("[check] ⚠ rsync SANS support ACL/xattr : l'option 'Backup système fidèle' ne préservera PAS les métadonnées. Vérifie que les paquets acl+attr sont installés.")
	}
}

func apiJobs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		jobsMu.Lock()
		list := make([]*Job, 0, len(jobs))
		for _, j := range jobs {
			list = append(list, j)
		}
		jobsMu.Unlock()
		sort.Slice(list, func(a, b int) bool { return list[a].ID < list[b].ID })
		writeJSON(w, list)
	case "POST":
		var j Job
		if json.NewDecoder(r.Body).Decode(&j) != nil || j.Name == "" {
			http.Error(w, "champs requis manquants", 400)
			return
		}
		if j.Kind != "command" && (j.Source == "" || j.Dest == "") {
			http.Error(w, "source et destination requises", 400)
			return
		}
		if err := validateJob(&j); err != "" {
			http.Error(w, err, 400)
			return
		}
		jobsMu.Lock()
		j.ID = nextID
		nextID++
		defaults(&j)
		jobs[j.ID] = &j
		jobsMu.Unlock()
		save()
		schedule(&j)
		writeJSON(w, j)
	default:
		http.Error(w, "méthode", 405)
	}
}

// validateJob : refuse les configurations dangereuses ou incohérentes.
// Renvoie "" si OK, sinon un message d'erreur.
func validateJob(j *Job) string {
	// Backend System : n'a de sens que pour un déclencheur autonome (cron/watch).
	// Le manuel reste forcément côté SyncBridge (c'est SyncBridge qui "lance").
	if j.Backend == "system" && j.Trigger != "cron" && j.Trigger != "watch" {
		return "backend System : choisis un déclencheur cron ou surveillance (le manuel reste côté SyncBridge)"
	}
	if j.Kind == "command" {
		if strings.TrimSpace(j.Command) == "" {
			return "job commande : la commande est vide"
		}
		if j.Trigger == "cron" && len(strings.Fields(j.Cron)) != 5 {
			return "expression cron invalide : 5 champs attendus (min heure jour mois jour-semaine)"
		}
		return ""
	}
	src := strings.TrimRight(j.Source, "/")
	dst := strings.TrimRight(j.Dest, "/")
	if src == dst && src != "" {
		return "source et destination identiques : refusé (risque de perte de données)"
	}
	// destination imbriquée dans la source en mode miroir = --delete catastrophique
	if j.Mode == "mirror" && src != "" && strings.HasPrefix(dst+"/", src+"/") {
		return "en miroir, la destination ne peut pas être à l'intérieur de la source"
	}
	if j.Trigger == "cron" && len(strings.Fields(j.Cron)) != 5 {
		return "expression cron invalide : 5 champs attendus (min heure jour mois jour-semaine)"
	}
	return ""
}

func defaults(j *Job) {
	if j.Kind == "" {
		j.Kind = "sync"
	}
	if j.Backend == "" {
		j.Backend = "syncbridge"
	}
	if j.Engine == "" {
		j.Engine = "rsync"
	}
	if j.Mode == "" {
		j.Mode = "add"
	}
	if j.Trigger == "" {
		j.Trigger = "manual"
	}
	if j.Compare == "" {
		j.Compare = "time"
	}
}

func apiJobByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/jobs/")
	parts := strings.Split(rest, "/")
	id, err := strconv.Atoi(parts[0])
	if err != nil {
		http.Error(w, "id", 400)
		return
	}

	// sous-routes : /run, /stream
	if len(parts) == 2 {
		switch parts[1] {
		case "run":
			dry := r.URL.Query().Get("dry") == "1"
			runsMu.Lock()
			busy := runs[id] != nil && runs[id].Running
			runsMu.Unlock()
			if busy {
				http.Error(w, "déjà en cours", 409)
				return
			}
			go execJob(id, dry)
			writeJSON(w, map[string]any{"started": id, "dry": dry})
			return
		case "stream":
			streamLogs(w, r, id)
			return
		case "clone":
			jobsMu.Lock()
			orig := jobs[id]
			if orig == nil {
				jobsMu.Unlock()
				http.Error(w, "introuvable", 404)
				return
			}
			cp := *orig
			cp.ID = nextID
			nextID++
			cp.Name = orig.Name + " (copie)"
			cp.LastRun, cp.LastStat = "", ""
			cp.Disabled = true // clone créé désactivé par sécurité
			jobs[cp.ID] = &cp
			jobsMu.Unlock()
			save()
			writeJSON(w, cp)
			return
		case "toggle":
			jobsMu.Lock()
			j := jobs[id]
			if j == nil {
				jobsMu.Unlock()
				http.Error(w, "introuvable", 404)
				return
			}
			j.Disabled = !j.Disabled
			dis := j.Disabled
			jobsMu.Unlock()
			save()
			schedule(jobs[id])
			writeJSON(w, map[string]any{"id": id, "disabled": dis})
			return
		case "kill":
			killed := killJob(id)
			writeJSON(w, map[string]any{"id": id, "killed": killed})
			return
		}
	}

	switch r.Method {
	case "PUT":
		var in Job
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			http.Error(w, "json", 400)
			return
		}
		if err := validateJob(&in); err != "" {
			http.Error(w, err, 400)
			return
		}
		jobsMu.Lock()
		j := jobs[id]
		if j == nil {
			jobsMu.Unlock()
			http.Error(w, "introuvable", 404)
			return
		}
		in.ID = id
		in.LastRun, in.LastStat = j.LastRun, j.LastStat
		defaults(&in)
		jobs[id] = &in
		jobsMu.Unlock()
		save()
		schedule(&in)
		writeJSON(w, in)
	case "DELETE":
		jobsMu.Lock()
		j := jobs[id]
		delete(jobs, id)
		jobsMu.Unlock()
		if cid, ok := cronIDs[id]; ok {
			sched.Remove(cid)
			delete(cronIDs, id)
		}
		stopWatch(id)
		if j != nil {
			removeSystemArtifact(j) // nettoie tout cron.d/systemd posé sur l'hôte
		}
		save()
		writeJSON(w, map[string]int{"deleted": id})
	default:
		http.Error(w, "méthode", 405)
	}
}

func streamLogs(w http.ResponseWriter, r *http.Request, id int) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flush", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	idx := 0
	for {
		runsMu.Lock()
		run := runs[id]
		runsMu.Unlock()
		if run == nil {
			fmt.Fprintf(w, "event: done\ndata: {}\n\n")
			fl.Flush()
			return
		}
		run.mu.Lock()
		lines := append([]string(nil), run.Lines...)
		running := run.Running
		rc := run.RC
		run.mu.Unlock()

		for idx < len(lines) {
			b, _ := json.Marshal(strings.TrimRight(lines[idx], "\n"))
			fmt.Fprintf(w, "data: %s\n\n", b)
			idx++
		}
		fl.Flush()
		if !running && idx >= len(lines) {
			fmt.Fprintf(w, "event: done\ndata: {\"rc\":%d}\n\n", rc)
			fl.Flush()
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(400 * time.Millisecond):
		}
	}
}

func apiBrowse(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "/mnt"
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.IsDir() {
		http.Error(w, "dossier invalide", 400)
		return
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		http.Error(w, "accès refusé", 403)
		return
	}
	dirs := []string{}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)
	parent := ""
	if path != "/" {
		parent = filepath.Dir(path)
	}
	writeJSON(w, map[string]any{"path": path, "parent": parent, "dirs": dirs})
}
