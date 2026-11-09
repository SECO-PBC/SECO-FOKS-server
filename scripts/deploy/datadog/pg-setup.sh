#!/usr/bin/env bash
# One-time Postgres setup for Datadog Database Monitoring.
#
# Creates a least-privilege `datadog` role (pg_monitor) and, in every foks_*
# database plus `postgres`, the pg_stat_statements extension and the
# datadog.explain_statement() helper DBM uses for explain plans.
#
# Prerequisite: postgres started with
#   -c shared_preload_libraries=pg_stat_statements
# (see scripts/deploy/README.md, "Telemetry (Datadog)").
#
# Usage, on the server:
#   DD_PG_PASSWORD=... ./pg-setup.sh [container]   # default workdir-postgresql-1
#
# Idempotent: safe to re-run (e.g. after a new foks_* database appears).
set -euo pipefail

CONTAINER="${1:-workdir-postgresql-1}"
: "${DD_PG_PASSWORD:?set DD_PG_PASSWORD (same value as in .env)}"

psql_in() {
    local db="$1"
    shift
    docker exec -i "$CONTAINER" psql -v ON_ERROR_STOP=1 -U postgres -d "$db" "$@"
}

preload="$(psql_in postgres -tAc 'SHOW shared_preload_libraries')"
case ",${preload// /}," in
    *,pg_stat_statements,*) ;;
    *)
        echo "error: pg_stat_statements is not in shared_preload_libraries ('$preload')." >&2
        echo "Add it to the postgresql command and restart postgres first." >&2
        exit 1
        ;;
esac

# Role: create or reset the password. Passed as a psql variable so it is
# quoted by psql, not interpolated into SQL by the shell.
psql_in postgres -v pw="$DD_PG_PASSWORD" <<'SQL'
SELECT format('CREATE ROLE datadog WITH LOGIN PASSWORD %L', :'pw')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'datadog') \gexec
SELECT format('ALTER ROLE datadog WITH LOGIN PASSWORD %L', :'pw') \gexec
GRANT pg_monitor TO datadog;
SQL

dbs="$(psql_in postgres -tAc "SELECT datname FROM pg_database WHERE datname = 'postgres' OR datname LIKE 'foks\_%' ORDER BY 1")"

for db in $dbs; do
    echo "configuring $db"
    psql_in "$db" <<'SQL'
CREATE SCHEMA IF NOT EXISTS datadog;
GRANT USAGE ON SCHEMA datadog TO datadog;
GRANT USAGE ON SCHEMA public TO datadog;
CREATE EXTENSION IF NOT EXISTS pg_stat_statements SCHEMA public;

CREATE OR REPLACE FUNCTION datadog.explain_statement(
    l_query TEXT,
    OUT explain JSON
)
RETURNS SETOF JSON AS
$$
DECLARE
    curs REFCURSOR;
    plan JSON;
BEGIN
    OPEN curs FOR EXECUTE pg_catalog.concat('EXPLAIN (FORMAT JSON) ', l_query);
    FETCH curs INTO plan;
    CLOSE curs;
    RETURN QUERY SELECT plan;
END;
$$
LANGUAGE 'plpgsql'
RETURNS NULL ON NULL INPUT
SECURITY DEFINER;
SQL
done

echo "done: datadog role + DBM helpers in: $(echo $dbs | tr '\n' ' ')"
