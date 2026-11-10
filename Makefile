.PHONY: env up down restart logs ps reset build sh-worker sh-gateway prod-up prod-down prod-logs k8s-dry test check-js backup restore

env:
	@test -f .env || cp .env.example .env

up: env
	docker compose up -d --build

down:
	docker compose down

restart:
	docker compose restart

logs:
	docker compose logs -f --tail=100

ps:
	docker compose ps

reset:
	docker compose down -v

build:
	docker compose build

sh-worker:
	docker compose exec sounder-worker bash

sh-gateway:
	docker compose exec sounder-gateway bash

prod-up:
	docker compose -f docker-compose.prod.yml up -d

prod-down:
	docker compose -f docker-compose.prod.yml down

prod-logs:
	docker compose -f docker-compose.prod.yml logs -f --tail=100

k8s-dry:
	kubectl kustomize deploy/k8s > /tmp/sounder-k8s.yaml
	@echo "rendered /tmp/sounder-k8s.yaml"

test:
	cd sounder-server && go test ./...
	cd sounder-worker && go test ./...
	cd sounder-gateway && go test ./...

check-js:
	./scripts/check-js.sh

backup:
	./scripts/backup.sh

restore:
	@test -n "$(BACKUP)" || (echo "用法: make restore BACKUP=backups/20260101-120000" && exit 1)
	./scripts/restore.sh "$(BACKUP)"
