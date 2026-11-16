-- Requires the v2 schema in wendylabsinc/cloud#798. Use psql -v start=... -v end=... -v as_of=...
-- Counts observed run attempts, not people or unique applications. Missing readiness is unknown.
WITH events AS MATERIALIZED (
 SELECT anonymous_id, attempt_id, event, occurred_at, success, error_class, properties,
        is_dev_build OR cli_version LIKE 'dev-%' AS development
 FROM cli_events
 WHERE schema_version = 2 AND attempt_id IS NOT NULL
   AND occurred_at >= :'start'::timestamptz AND occurred_at < :'end'::timestamptz
   AND occurred_at < :'as_of'::timestamptz AND created_at < :'as_of'::timestamptz
   AND command_name IN ('wendy run', 'wendy watch')
   AND event IN ('run_started', 'run_finished', 'app_started', 'app_ready', 'app_readiness_failed')
), attempts AS (
 SELECT anonymous_id, attempt_id, bool_or(development) AS development,
        min(occurred_at) AS first_observed,
        min(occurred_at) FILTER (WHERE event = 'run_started') AS began,
        min(occurred_at) FILTER (WHERE event = 'app_started') AS started,
        min(occurred_at) FILTER (WHERE event = 'app_ready') AS ready,
        min(occurred_at) FILTER (WHERE event IN ('run_finished', 'app_readiness_failed')
          AND (properties->>'outcome' = 'failed' OR
            (properties->>'outcome' IS NULL AND NOT success
              AND coalesce(error_class, '') NOT IN ('context_canceled', 'user_cancelled', 'cancelled')))) AS failed,
        bool_or(properties->>'outcome' = 'cancelled' OR
                coalesce(error_class, '') IN ('context_canceled', 'user_cancelled', 'cancelled')) AS cancelled,
        bool_or(event = 'run_finished') AS finished
 FROM events GROUP BY anonymous_id, attempt_id
), observations AS (
 SELECT a.*, row_number() OVER (PARTITION BY anonymous_id ORDER BY first_observed, attempt_id) AS observed_attempt_number,
   bool_or(development) OVER (PARTITION BY anonymous_id) AS observed_dev_history,
   EXISTS (SELECT 1 FROM attempts later WHERE later.anonymous_id = a.anonymous_id
           AND later.ready > a.failed) AS later_readiness
 FROM attempts a
)
SELECT CASE WHEN observed_dev_history THEN 'observed_development' ELSE 'no_observed_development' END AS segment,
 count(*) AS observed_attempts,
 count(DISTINCT anonymous_id) AS installation_ids,
 count(*) FILTER (WHERE started IS NOT NULL) AS app_started_attempts,
 count(*) FILTER (WHERE ready IS NOT NULL) AS ready_attempts,
 count(*) FILTER (WHERE observed_attempt_number = 1 AND ready IS NOT NULL) AS installations_ready_on_first_observed_attempt,
 count(*) FILTER (WHERE failed IS NOT NULL) AS failed_attempts,
 count(*) FILTER (WHERE cancelled) AS cancelled_attempts,
 count(*) FILTER (WHERE NOT finished) AS attempts_without_terminal_event,
 count(*) FILTER (WHERE failed IS NOT NULL AND later_readiness) AS failures_with_later_readiness_observation,
 count(DISTINCT anonymous_id) FILTER (WHERE failed IS NOT NULL AND later_readiness) AS installations_with_later_readiness,
 round((percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM ready-began))
   FILTER (WHERE ready >= began))::numeric, 2) AS median_seconds_to_ready
FROM observations GROUP BY 1 ORDER BY 1;
