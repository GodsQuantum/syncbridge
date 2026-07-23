# SyncBridge — image auto-portée (contexte = racine du dépôt).
# Publiée en multi-arch sur ghcr.io par .github/workflows/docker-publish.yml.
# --- build ---
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY . .
RUN go mod download && \
    CGO_ENABLED=0 go build -ldflags="-s -w" -o /syncbridge .

# --- runtime ---
FROM alpine:3.20
# rsync/rclone : moteurs de sync. acl/attr : backup système fidèle (xattr).
# bash/curl/jq/findutils : exécution des scripts (jobs command, backend interne).
# docker-cli + compose : cibles Module 1 (prune, compose pull, docker exec export).
# openssh-client : Module SSH (piloter un autre serveur).
RUN apk add --no-cache \
    rsync rclone tzdata ca-certificates acl attr \
    bash curl jq findutils \
    docker-cli docker-cli-compose \
    openssh-client
COPY --from=build /syncbridge /usr/local/bin/syncbridge
EXPOSE 8787
ENTRYPOINT ["/usr/local/bin/syncbridge"]
