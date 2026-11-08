.PHONY: env up down restart logs ps reset build sh-worker sh-gateway prod-up prod-down prod-logs k8s-dry

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
