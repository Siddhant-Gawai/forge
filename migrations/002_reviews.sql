ALTER TABLE forge.merge_requests ADD COLUMN remote jsonb;
ALTER TABLE forge.review_comments ADD COLUMN commit_sha text NOT NULL DEFAULT '';
ALTER TABLE forge.review_comments ADD COLUMN side text NOT NULL DEFAULT '';
UPDATE forge.schema_version SET version=2, revision=revision+1 WHERE id=1;
