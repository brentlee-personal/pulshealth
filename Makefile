# Internal personal-account services. Ingress and backup have separate gates.
COMPOSE       := docker compose --project-directory server -f server/docker-compose.yml
COMPOSE_BUILD := $(COMPOSE) -f server/compose.build.yml
ARGS          ?=

.PHONY: help bootstrap up down pull logs ps migrate baseline pairing issue-device devices web-invite web-keys dev-up \
        backup backup-list restore site-dev site-build site-lint blog-check deploy-site

help: ## List targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F ':.*## ' '{ printf "  %-12s %s\\n", $$1, $$2 }'

bootstrap: ## Create non-secret settings and build internal services (ARGS='--url https://<private-origin>')
	scripts/bootstrap.sh $(ARGS)

up: ## Start already-built images for this exact checkout
	DEPLOY_COMMIT=$$(git rev-parse HEAD) $(COMPOSE) up -d $(ARGS)

down: ## Stop this stack and preserve its volumes
	$(COMPOSE) down $(ARGS)

pull: ## Published upstream shared-mode images are incompatible
	@echo 'Build this checkout with make dev-up. Image release is a separate gate.' >&2; exit 1

logs: ## Follow bounded logs (ARGS=<service>)
	$(COMPOSE) logs -f --tail=200 $(ARGS)

ps: ## Container status
	$(COMPOSE) ps $(ARGS)

migrate: ## Apply pending migrations using the one-shot protected-file role
	$(COMPOSE) run --rm migrate

baseline: ## Adopt an approved restored schema before applying pending migrations
	$(COMPOSE) run --rm migrate baseline

pairing: ## Pair through the signed-in member account
	@echo 'Shared pairing is retired. Use the member account phone-pairing page.' >&2; exit 1

issue-device: ## Explicit member only (NAME='<label>' USER_ID='<member UUID>')
	@test -n "$(NAME)" -a -n "$(USER_ID)" || { echo "NAME and USER_ID are required" >&2; exit 2; }
	$(COMPOSE) run --rm --no-deps ingest devices issue --user "$(USER_ID)" --name "$(NAME)" $(ARGS)

devices: ## Explicit device commands (ARGS='list|issue --user <uuid> --name <label>|revoke <id>|rename <id> <label>')
	@$(if $(strip $(ARGS)),:,echo "Explicit device command required" >&2; exit 2)
	$(COMPOSE) run --rm --no-deps ingest devices $(ARGS)

web-invite: ## Invite a new member (ARGS='--email <address> [--admin] [--send]')
	@$(if $(strip $(ARGS)),:,echo "Explicit invite arguments required" >&2; exit 2)
	$(COMPOSE) exec web node scripts/invite.mjs $(ARGS)

web-keys: ## Create a new signing key without replacing existing files (ARGS='key-v2')
	@$(if $(strip $(ARGS)),:,echo "A new key ID is required" >&2; exit 2)
	$(COMPOSE) exec web node scripts/auth-keys.mjs /auth-keys /auth-public $(ARGS)

dev-up: ## Build this checkout and start only its enabled internal services
	DEPLOY_COMMIT=$$(git rev-parse HEAD) $(COMPOSE_BUILD) up -d --build $(ARGS)

backup backup-list restore: ## Closed until encrypted backup and restore acceptance
	@echo 'Encrypted backup and restore acceptance is pending. No backup or deletion was run.' >&2; exit 1

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
