-- name: NextOriginalReclamation :one
SELECT r.hash, r.generation, b.size FROM original_reclamation r
JOIN blobs b USING (hash)
WHERE r.generation IS NOT NULL AND NOT (r.generation = ANY (COALESCE(sqlc.arg(deferred)::bigint[], '{}')))
ORDER BY r.generation LIMIT 1;

-- name: PendingOriginalGenerations :many
SELECT generation::bigint FROM original_reclamation
WHERE generation = ANY (sqlc.arg(generations)::bigint[]);

-- name: ReadOriginalReclamation :one
SELECT r.hash, r.generation, b.size FROM original_reclamation r
JOIN blobs b USING (hash)
WHERE r.hash = $1 AND r.generation IS NOT NULL;

-- name: OriginalHasReferences :one
SELECT EXISTS (SELECT FROM original_references WHERE hash = $1);

-- name: OriginalIsRegistered :one
SELECT EXISTS (SELECT FROM blobs WHERE hash = $1);

-- name: OriginalFileRequired :one
-- A committed reference, a reclamation row (live or eligible) or an armed
-- removal requires the hash's existing file. Only an orphan with none of
-- them, which collection never touches, may be pinned again. References
-- are checked besides the row, so a lost row still refuses.
SELECT EXISTS (
    SELECT 1 FROM original_references f WHERE f.hash = sqlc.arg(hash)::text
    UNION ALL SELECT 1 FROM original_reclamation r WHERE r.hash = sqlc.arg(hash)::text
    UNION ALL SELECT 1 FROM original_removal i WHERE i.hash = sqlc.arg(hash)::text);

-- name: ReadOriginalRemoval :one
SELECT store_id, hash, size, generation FROM original_removal;

-- name: MakeOriginalRemovalDurable :exec
SET LOCAL synchronous_commit = on;

-- name: ArmOriginalRemoval :exec
INSERT INTO original_removal (store_id, hash, size, generation)
VALUES ($1, $2, $3, $4);

-- name: ClearOriginalRemoval :exec
DELETE FROM original_removal WHERE singleton;

-- name: ClearOriginalReclamation :exec
DELETE FROM original_reclamation WHERE hash = $1;

-- name: DeleteOriginalRegistration :exec
DELETE FROM blobs WHERE hash = $1;

-- name: RestoreClearOriginalReclamation :exec
DELETE FROM original_reclamation;

-- name: RestoreSeedOriginalReclamation :exec
INSERT INTO original_reclamation (hash)
SELECT DISTINCT hash FROM original_references;
