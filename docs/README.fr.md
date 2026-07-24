# SyncBridge — Orchestrateur HomeLab

**Une seule interface web pour centraliser tes scripts Linux : tâches cron, surveillance de dossiers (inotify) et synchros rsync/rclone — avec logs en direct, garde-fous, et rien qui se passe dans ton dos.**

Une alternative légère et auto-hébergée aux crontabs éparpillés, aux boucles `inotifywait` en fond et aux scripts rsync jetables. En gros : *cronmaster + une web UI rsync + un gestionnaire d'inotify*, dans un seul binaire Go de 8 Mo. Pas de base de données, pas de build : tu tires l'image et c'est parti.

🇬🇧 [English README](../README.md)

---

## Pourquoi

Si ton homelab ressemble au mien — un dossier `CRON/` par-ci, un watcher `inotify/` par-là, une douzaine de one-liners rsync dont tu te souviens à moitié — SyncBridge rassemble tout ça au même endroit. Tu choisis un **déclencheur** (planifié, sur changement de dossier, ou à la demande) et une **action** (une synchro rsync/rclone, ou n'importe quelle commande/script), depuis une UI. Fini d'éditer les crontabs à la main en croisant les doigts.

Le mot d'ordre : **visibilité totale**. Rien ne s'exécute sans que tu le voies. Chaque exécution capture `stdout`/`stderr` dans un dashboard en direct, et les opérations dangereuses sont refusées avant de faire des dégâts.

## Fonctionnalités

- **Planificateur (cron)** — cron 5 champs standard pour lancer un script ou une commande (`docker system prune`, maj de stacks, export de base, diagnostics…).
- **Surveillance de dossier** — surveille un dossier, filtre par motif (`*.mp4`), déclenche une action. **NFS-safe** : hybride inotify + polling, parce qu'inotify seul ne voit jamais les écritures distantes NFS.
- **Exécution manuelle** — lance des scripts utilitaires à la demande, en asynchrone, avec logs streamés et bouton d'arrêt.
- **Synchro rsync / rclone** — miroir / accumulation / déplacement, comparaison checksum ou date, limite de débit, exclusions, corbeille rotative, backup système fidèle (ACL/xattr).
- **Moniteur système** *(lecture seule)* — scanne les crontabs, unités systemd (`.service`/`.timer`/`.path`) et process `inotifywait` de l'hôte, pour que rien ne t'échappe. L'import permet aussi de **désactiver/réactiver de façon réversible** un déclencheur hôte (cron commenté `#SB-OFF#`, systemd via `systemctl`) et de **supprimer définitivement** (par lot, confirmation « delete », avec corbeille récupérable).
- **Pilotage multi-instances** — installe SyncBridge sur plusieurs machines et pilote-les toutes depuis une seule UI (comme les agents **Dockge**). Ajoute une instance distante par URL + ses identifiants ; le sélecteur en haut à droite pilote alors ses jobs, son import système et ses logs live via un reverse-proxy intégré. On ne peut pas monter les volumes *système* d'un autre serveur sur le réseau — donc on parle à son SyncBridge. Les identifiants sont stockés sur l'instance pilote (`/config`, `0600`) pour se reconnecter automatiquement.

## La sécurité d'abord

SyncBridge refuse de te laisser te tirer une balle dans le pied :

- **Garde source vide/démontée** — le grand classique de la catastrophe rsync : un miroir dont la source NFS a été démontée paraît *vide*, et `--delete` efface la destination. SyncBridge **abandonne** le run à la place.
- **Verrou anti-chevauchement** — un job ne s'empile jamais sur lui-même (watchers rapides, cron qui déborde sur son propre intervalle).
- **Timeout** — un job qui pend est tué après N secondes (tout le groupe de process) : un job bloqué à vie est pire qu'un job qui échoue.
- **Garde-fous suppression** — le miroir refuse `source == destination` et destination-dans-source ; abandon possible au-delà de N suppressions.
- **Vérif fin d'écriture** — un watcher attend que le dossier soit calme (taille stable) avant de déclencher, pour ne pas synchroniser un fichier à moitié copié.

## Installation (image pré-buildée, sans build)

```bash
mkdir syncbridge && cd syncbridge
curl -O https://raw.githubusercontent.com/GodsQuantum/SyncBridge/main/compose.example.yaml
curl -O https://raw.githubusercontent.com/GodsQuantum/SyncBridge/main/.env.example
cp .env.example .env
# ajuste les volumes dans compose.example.yaml à tes chemins, puis :
docker compose -f compose.example.yaml up -d
```

Ouvre `http://<ip-serveur>:8788`. Tes jobs vivent dans `./data/jobs.json` — sauvegarde ce dossier et tu as tout sauvegardé. L'image est multi-arch (amd64 + arm64) sur `ghcr.io/godsquantum/syncbridge:latest`.

### Pas à pas

1. **Crée le dossier de config** et récupère les deux fichiers ci-dessus.
2. **Rédige ton `.env`** — des valeurs par défaut correctes sont fournies ; la plupart ne changent que `TZ`.
3. **Choisis tes volumes** (bind mounts). Le seul obligatoire est `./data:/config`. Mappe les dossiers que tu synchronises, les scripts à déclencher (`:ro` suffit) et — si tu veux le moniteur système lecture-seule — les montages `/host/...`. Chaque ligne est documentée dans `compose.example.yaml`, y compris la socket Docker (jobs `docker`) et le montage `/mnt` en `rshared` (NFS imbriqués).
4. **`docker compose up -d`**, ouvre l'UI, crée ton premier job. Utilise le **dry-run** sur les synchros avant de leur faire confiance.

## Types de déclencheurs

| Déclencheur | Se lance quand | Exemple |
|---|---|---|
| Manuel | tu cliques Lancer | `YT.sh <url>` |
| Cron | à l'horaire | `0 4 * * *` → `docker system prune -f` |
| Watch | un dossier change | un `.mp4` arrive → une synchro rsync, ou un script ffmpeg |

## Backends d'exécution (au choix, par job)

Chaque job choisit où il s'exécute :

- **Backend SyncBridge** *(défaut)* — SyncBridge exécute lui-même : logs live, bouton kill, arrêt propre, verrou anti-chevauchement. Simple et totalement visible. S'arrête si le conteneur s'arrête.
- **Backend System** — SyncBridge écrit une **vraie ligne cron hôte** (`/etc/cron.d`, avec `flock` + `PATH`) ou une **unité systemd `.path`/`.service`** : le job **continue de tourner même si Docker/SyncBridge est down**. Invariant : l'artefact est retiré avant toute réécriture et nettoyé à la suppression ou au changement de backend — jamais de doublon invisible. L'écriture cron hôte demande un montage rw ; la variante systemd demande un conteneur privilégié (voir `compose.example.yaml`).

## Roadmap

Déclencheurs d'ingestion USB · historique par job plus riche · TLS optionnel entre instances pilotées.

## Licence

MIT — voir [LICENSE](../LICENSE). Contributions bienvenues.
