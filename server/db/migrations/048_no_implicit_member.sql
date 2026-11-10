-- Preserve existing records; new writes must name their authenticated member.
DO $$
DECLARE column_default record;
BEGIN
  FOR column_default IN
    SELECT n.nspname, c.relname
      FROM pg_attrdef d
      JOIN pg_attribute a ON a.attrelid = d.adrelid AND a.attnum = d.adnum
      JOIN pg_class c ON c.oid = a.attrelid
      JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p')
       AND a.attname = 'user_id' AND a.atttypid = 'uuid'::regtype
  LOOP
    EXECUTE format('ALTER TABLE %I.%I ALTER COLUMN user_id DROP DEFAULT', column_default.nspname, column_default.relname);
  END LOOP;
END
$$;
