-- +goose Up
-- +goose StatementBegin

-- Releases before the ipmitool command line was redacted stored a failed
-- BMC reconcile's error verbatim in operations.result, and the command line
-- carries passwords: the monitor password of `user set password <slot>
-- <password> [16|20]` and `user test <slot> <16|20> <password>`, and the
-- admin password after -P. The API returns result as stored. New rows are
-- redacted where the error is built (bmcreconcile.runIPMITool); this
-- rewrites the rows already in the table. A password is everything up to
-- the next space, colon, double quote or backslash, which is the alphabet
-- the generated passwords and the JSON string around them leave.
UPDATE operations
SET result = regexp_replace(
               regexp_replace(
                 regexp_replace(result::text,
                   '(user set password [0-9]+ )[^ :"\\]+', '\1<redacted>', 'g'),
                 '(user test [0-9]+ (16|20) )[^ :"\\]+', '\1<redacted>', 'g'),
               '( -P )[^ :"\\]+', '\1<redacted>', 'g')::jsonb
WHERE result IS NOT NULL
  AND result::text ~ '(user set password [0-9]+ |user test [0-9]+ (16|20) | -P )';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- The passwords are gone; nothing to restore.
SELECT 'down migration keeps the redaction' AS note;
-- +goose StatementEnd
