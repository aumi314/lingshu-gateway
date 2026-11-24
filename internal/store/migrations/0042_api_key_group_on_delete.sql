ALTER TABLE api_keys
    ALTER COLUMN group_id DROP NOT NULL,
    DROP CONSTRAINT api_keys_group_id_fkey,
    ADD CONSTRAINT api_keys_group_id_fkey FOREIGN KEY (group_id) REFERENCES groups(id) ON DELETE SET NULL;
