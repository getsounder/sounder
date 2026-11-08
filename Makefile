.PHONY: env up down restart logs ps reset build sh-worker sh-gateway

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
