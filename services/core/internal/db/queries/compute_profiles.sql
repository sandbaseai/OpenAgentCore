-- name: FreezeSessionComputeGeneration :exec
INSERT INTO session_compute_generations(session_id,profile,generation) VALUES($1,$2,$3);

-- name: GetSessionComputeGeneration :one
SELECT c.generation,g.specification,g.provider_kind FROM session_compute_generations c
LEFT JOIN runtime_deployment_generations g ON g.generation=c.generation WHERE c.session_id=$1;

-- name: SaveComputeProfileGeneration :exec
INSERT INTO runtime_deployment_generations(generation,provider_kind,specification,provider_config,provider_metadata)
VALUES($1,$2,$3,$4,$5);

-- name: ExpandComputeProfileSelections :one
SELECT coalesce(jsonb_object_agg(p.key,jsonb_build_object(
 'generation',g.generation,'specification',g.specification,
 'configuration',g.provider_config,'metadata',g.provider_metadata)), '{}'::jsonb)::jsonb AS profiles
FROM jsonb_each_text(sqlc.arg(profiles)::jsonb) p
JOIN runtime_deployment_generations g ON g.generation=p.value::bigint;
