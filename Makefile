.PHONY: up down wait tidy scenario1 scenario2 scenario3 scenario4 loadtest loadtest-small loadtest-server help

DSN_NORMAL  = "host=localhost port=5440 dbname=demo user=demo password=demo sslmode=disable"
DSN_LIMITED = "host=localhost port=5441 dbname=demo user=demo password=demo sslmode=disable"

help:
	@echo ""
	@echo "PostgreSQL Connection Pool Tuning Demo"
	@echo "======================================="
	@echo ""
	@echo "Infrastructure:"
	@echo "  make up          Start both PostgreSQL containers"
	@echo "  make down        Stop and remove containers"
	@echo "  make wait        Wait until both containers are healthy"
	@echo "  make tidy        Download Go dependencies"
	@echo ""
	@echo "Scenarios:"
	@echo "  make scenario1   Server max_connections=10 vs 15 workers"
	@echo "  make scenario2   App pool=3 vs 9 workers (degrade + error)"
	@echo "  make scenario3   MaxIdleConns and ConnMaxIdleTime"
	@echo "  make scenario4   All 5 timeout parameters"
	@echo ""
	@echo "Load tests:"
	@echo "  make loadtest          Default: pool=5, workers=20, qtime=0.1s"
	@echo "  make loadtest-ok       Healthy: pool=20, workers=20 (no contention)"
	@echo "  make loadtest-small    Saturated: pool=3, workers=20 (high wait)"
	@echo "  make loadtest-server   Hit server limit: pool=20, db=pg-limited"
	@echo ""

up:
	docker compose up -d
	@echo "Containers started. Run 'make wait' to confirm both are healthy."

down:
	docker compose down

wait:
	@echo "Waiting for postgres-normal  (port 5432)..."
	@until docker exec pg-normal pg_isready -U demo -d demo > /dev/null 2>&1; do sleep 1; done
	@echo "  postgres-normal  is ready."
	@echo "Waiting for postgres-limited (port 5433)..."
	@until docker exec pg-limited pg_isready -U demo -d demo > /dev/null 2>&1; do sleep 1; done
	@echo "  postgres-limited is ready."
	@echo ""
	@echo "Both databases are ready. Run any 'make scenarioN' command."

tidy:
	go mod tidy

scenario1:
	go run ./scenarios/01_server_max_conn/

scenario2:
	go run ./scenarios/02_pool_exhaustion/

scenario3:
	go run ./scenarios/03_idle_connections/

scenario4:
	go run ./scenarios/04_timeouts/

loadtest:
	go run ./loadtest/ -pool 5 -workers 20 -qtime 0.1 -dur 15s

loadtest-ok:
	go run ./loadtest/ -pool 20 -workers 20 -qtime 0.1 -dur 15s

loadtest-small:
	go run ./loadtest/ -pool 3 -workers 20 -qtime 0.1 -dur 15s

loadtest-server:
	go run ./loadtest/ -pool 20 -workers 20 -qtime 0.5 -dur 15s -db $(DSN_LIMITED)
