-- AIshiteru Core — after the newest migrations went down and up again, in
-- `make db-test-sql`
--
-- A down may leave rows standing that the schema before it holds — 0007's
-- leaves the built-in presets delegate and course_tutor — and the up after
-- it must bring them up to date: the seed leaves a built-in that exists as
-- it is. So the built-ins, as the seed left them before the down, must come
-- through the down and the up unchanged.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
DECLARE
    changed text;
BEGIN
    SELECT string_agg(r.name, ', ' ORDER BY r.name) INTO changed
      FROM redo_builtins r
      LEFT JOIN permission_preset p ON p.id = r.id
     WHERE p.id IS NULL OR to_jsonb(p) <> to_jsonb(r);
    IF changed IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  down and up again: the built-in presets % are not as the seed left them', changed;
    END IF;
END $chk$;
DROP TABLE redo_builtins;
\echo 'PASS  the built-in presets come through a down and an up as the seed left them'
