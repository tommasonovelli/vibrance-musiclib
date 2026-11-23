-- +goose Up

-- NULL marks a hash with a committed reference. A generation is issued
-- only when that hash loses its last committed reference.
CREATE SEQUENCE original_reclamation_generation;
CREATE TABLE original_reclamation (
    hash text PRIMARY KEY REFERENCES blobs ON DELETE RESTRICT,
    generation bigint CHECK (generation > 0)
);
CREATE UNIQUE INDEX original_reclamation_pending_idx
    ON original_reclamation (generation) WHERE generation IS NOT NULL;

CREATE TABLE original_removal (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    store_id uuid NOT NULL,
    hash text NOT NULL UNIQUE REFERENCES original_reclamation ON DELETE RESTRICT,
    size bigint NOT NULL CHECK (size >= 0),
    generation bigint NOT NULL CHECK (generation > 0)
);

CREATE VIEW original_references AS
    SELECT blob_hash AS hash FROM tracks
    UNION ALL SELECT lyrics_hash FROM tracks WHERE lyrics_hash IS NOT NULL
    UNION ALL SELECT blob_hash FROM attachments
    UNION ALL SELECT cover_hash FROM albums WHERE cover_hash IS NOT NULL;

INSERT INTO original_reclamation (hash)
    SELECT DISTINCT hash FROM original_references;

-- +goose StatementBegin
CREATE FUNCTION track_original_references() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    previous jsonb;
    current_row jsonb;
    field text;
    affected text;
BEGIN
    IF TG_OP <> 'INSERT' THEN previous := to_jsonb(OLD); END IF;
    IF TG_OP <> 'DELETE' THEN current_row := to_jsonb(NEW); END IF;
    FOREACH field IN ARRAY TG_ARGV LOOP
        IF previous->>field IS NOT DISTINCT FROM current_row->>field THEN
            CONTINUE;
        END IF;
        FOREACH affected IN ARRAY ARRAY[previous->>field, current_row->>field] LOOP
            IF affected IS NULL THEN CONTINUE; END IF;
            IF EXISTS (SELECT FROM original_references WHERE hash = affected) THEN
                INSERT INTO original_reclamation (hash) VALUES (affected)
                    ON CONFLICT (hash) DO UPDATE SET generation = NULL
                    WHERE original_reclamation.generation IS NOT NULL;
            ELSE
                UPDATE original_reclamation SET generation = nextval('original_reclamation_generation')
                    WHERE hash = affected AND generation IS NULL;
            END IF;
        END LOOP;
    END LOOP;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- Inspect the final references, including cascades and changes through
-- multiple roles in the same catalog transaction.
CREATE CONSTRAINT TRIGGER original_references_tracks
    AFTER INSERT OR UPDATE OF blob_hash, lyrics_hash OR DELETE ON tracks
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION track_original_references('blob_hash', 'lyrics_hash');
CREATE CONSTRAINT TRIGGER original_references_attachments
    AFTER INSERT OR UPDATE OF blob_hash OR DELETE ON attachments
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION track_original_references('blob_hash');
CREATE CONSTRAINT TRIGGER original_references_covers
    AFTER INSERT OR UPDATE OF cover_hash OR DELETE ON albums
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION track_original_references('cover_hash');
