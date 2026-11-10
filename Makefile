# Convenience targets for the reference server stack (server/docker-compose.yml)
# and the pulshealth.com marketing site (site/).
# Every stack target runs Compose against server/ so it works from the repository
# root; ARGS passes extra flags through (e.g. `make bootstrap ARGS=--lan`,
# `make logs ARGS=ingest`).

COMPOSE       := docker compose --project-directory server -f server/docker-compose.yml
COMPOSE_BUILD := $(COMPOSE) -f server/compose.build.yml
ARGS          ?=

.PHONY: help bootstrap up down pull logs ps migrate baseline pairing issue-device devices web-invite web-demo dev-up \
        backup backup-list restore site-dev site-build site-lint blog-check deploy-site

help: ## List targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F ':.*## ' '{ printf "  %-12s %s\n", $$1, $$2 }'

bootstrap: ## First run: create server/.env, start the stack, print the pairing block (ARGS=--lan|--url ...|--time-zone ...)
	scripts/bootstrap.sh $(ARGS)

up: ## Start (or update) the stack from the published images; migrate runs first
	$(COMPOSE) up -d $(ARGS)

down: ## Stop the stack (data volumes are kept; `down -v` would destroy them)
	$(COMPOSE) down $(ARGS)

pull: ## Pull the images for PULS_VERSION (then `make up` to switch to them)
	$(COMPOSE) pull $(ARGS)

logs: ## Follow logs (ARGS=<service> for one service)
	$(COMPOSE) logs -f --tail=200 $(ARGS)

ps: ## Container status
	$(COMPOSE) ps $(ARGS)

migrate: ## Apply pending schema migrations by hand (also runs on every `up`)
	$(COMPOSE) run --rm migrate

baseline: ## Adopt a database created before the migrate service existed (one-time)
	$(COMPOSE) run --rm migrate baseline

pairing: ## Re-print the pairing block (URL, token, user ID, QR) from server/.env
	scripts/bootstrap.sh --print-pairing

issue-device: ## Issue a per-device token and print its pairing QR: NAME='My iPhone' (ARGS='--user <uuid>' for a non-default user)
	@$(if $(strip $(NAME)),:,echo "usage: make issue-device NAME='<label, e.g. My iPhone>' [ARGS='--user <uuid>']"; exit 2)
	scripts/bootstrap.sh --issue-device "$(NAME)" $(ARGS)

# `issue` prints a QR code too, and the code needs the URL the phone will use,
# which the container cannot work out — so it is handed in as PULS_PUBLIC_URL,
# from the same rules the pairing block uses (the .env value, else the LAN
# address under --lan; empty when there is none, and `issue` then says so).
# An explicit --url in ARGS still wins. The emptiness check is make's, not the
# shell's `test -n "$(ARGS)"`: a quoted label (--name "My iPhone") inside those
# quotes splits into words and `test` fails on a perfectly good command.
devices: ## Per-device tokens: ARGS='list [--all]' | 'issue --user <uuid> --name <label> [--url <url>] [--no-qr]' | 'revoke <id>' | 'rename <id> <label>'
	@$(if $(strip $(ARGS)),:,echo "usage: make devices ARGS='list|issue --user <uuid> --name <label>|revoke <id>|rename <id> <label>'"; exit 2)
	$(COMPOSE) run --rm --no-deps -e PULS_PUBLIC_URL="$$(scripts/bootstrap.sh --print-url 2>/dev/null || true)" ingest devices $(ARGS)

# Accounts mode only (WEB_ACCOUNTS=true): a one-time link that creates the
# person's new member and viewer account. Runs inside the running web
# container, so it uses that container's database role and WEB_PUBLIC_URL.
web-invite: ## Invite a new member: ARGS='--email <address> [--admin] [--send]'
	@$(if $(strip $(ARGS)),:,echo "usage: make web-invite ARGS='--email <address> [--admin] [--url https://<viewer host>]'"; exit 2)
	$(COMPOSE) exec web node scripts/invite.mjs $(ARGS)

# The account behind the public demo (accounts mode, WEB_DEMO_USER): no
# password signs in to it, /demo does. The user must exist first and hold only
# sample data — `make issue-device NAME='Demo data' ARGS='--user <uuid>'`
# creates it and the loader's token. Prints the WEB_DEMO_USER line for .env.
web-demo: ## Create the public demo's view-only account (accounts mode): ARGS='--user <uuid> [--email demo@demo.invalid]'
	@$(if $(strip $(ARGS)),:,echo "usage: make web-demo ARGS='--user <uuid> [--email demo@demo.invalid]'"; exit 2)
	$(COMPOSE) exec web node scripts/demo.mjs $(ARGS)

dev-up: ## Build the four app images from this checkout and start the stack
	DEPLOY_COMMIT=$$(git rev-parse HEAD 2>/dev/null || echo unknown) $(COMPOSE_BUILD) up -d --build $(ARGS)

backup: ## Take one database dump now (scheduled dumps: docker compose --profile backup up -d backup)
	$(COMPOSE) --profile backup run --rm backup once

backup-list: ## List the dumps in the backup store
	$(COMPOSE) --profile backup run --rm backup list

restore: ## Restore a dump, DESTROYING the current database (FILE=<path or name from backup-list>)
	@test -n "$(FILE)" || { echo "usage: make restore FILE=<dump path, or a name from 'make backup-list'>"; exit 1; }
	server/backup/restore.sh $(ARGS) "$(FILE)"

# The marketing site (pulshealth.com). Distinct from web/, the self-hosted
# viewer: site/ is a static export that reads knowledge-base/ and blog/ as
# repository-root siblings, so these run from the root like everything else.
site-dev: ## Marketing site dev server on :3000 (site/, needs bun)
	cd site && bun install && bun run dev $(ARGS)

site-build: ## Static export of the marketing site to site/out
	cd site && bun install && bun run build

site-lint: ## ESLint the marketing site
	cd site && bun install && bun run lint

blog-check: ## Validate blog metadata, drafts, links and production export
	cd site && bun install --frozen-lockfile && bun run blog:check

deploy-site: ## Build the marketing site and publish it to S3 + CloudFront (ARGS=--dry-run)
	scripts/deploy-site.sh $(ARGS)

.PHONY: task-status ios-test verify-backend

task-status: ## Read-only worktree ownership, Git and PR status
	python3 scripts/task-status.py --github

ios-test: ## Test on an owned simulator; ARGS='--ios 26.5 --suite package'
	python3 scripts/test-ios.py $(ARGS)

verify-backend: ## Verify deployed revisions and real chart reads (ARGS='--expected-sha <SHA>')
	python3 server/ops/verify-release.py $(ARGS)
