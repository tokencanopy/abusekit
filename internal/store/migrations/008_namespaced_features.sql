-- P1 is intentionally a one-time key migration. Runtime accepts no flat aliases.
ALTER TABLE corpus_examples ADD COLUMN feature_key_space text NOT NULL DEFAULT 'flat-v0';
ALTER TABLE verdicts ADD COLUMN reason_version integer NOT NULL DEFAULT 1;
CREATE TEMP TABLE abusekit_feature_rename(old_name text PRIMARY KEY, new_name text UNIQUE) ON COMMIT DROP;
INSERT INTO abusekit_feature_rename VALUES
('burst_ratio_24h_vs_lifetime','core.burst_ratio_24h_vs_lifetime'),
('declines_before_first_success','core.declines_before_first_success'),
('distinct_recipients_1h','email.distinct_recipients_1h'),
('fingerprint_seen_on_other_subjects','core.fingerprint_seen_on_other_subjects'),
('first_day_distinct_domains','email.first_day_distinct_domains'),
('first_funding_prepaid','core.first_funding_prepaid'),
('key_total','core.credential_total'),
('key_velocity_1h','core.credential_velocity_1h'),
('linked_deleted_n','core.linked_deleted_n'),
('linked_labelled_abusive_n','core.linked_labelled_abusive_n'),
('name_brand_match','brand.name_match'),
('name_has_at','brand.name_has_at'),
('neighbors_truncated','core.neighbors_truncated'),
('resource_total','core.resource_total'),
('resource_velocity_1h','core.resource_velocity_1h'),
('self_send_before_external','email.self_send_before_external'),
('sends_10m_max','email.sends_10m_max'),
('sends_1h','email.sends_1h'),
('sends_first_day','email.sends_first_day'),
('subject_age_h','core.subject_age_h'),
('subject_brand_match','email.subject_brand_match'),
('upgrade_delay_min','core.upgrade_delay_min'),
('upgraded','core.upgraded'),
('webmail_recipient_share','email.webmail_recipient_share'),
('webmail_sends_1h','email.webmail_sends_1h');
DO $$ BEGIN
 IF EXISTS (
   SELECT 1 FROM corpus_examples c CROSS JOIN LATERAL jsonb_each(COALESCE(NULLIF(c.features,'null'::jsonb),'{}'::jsonb)) e
   LEFT JOIN abusekit_feature_rename r ON r.old_name=e.key
   WHERE r.old_name IS NULL AND e.key !~ '^[a-z][a-z0-9]*[.][a-z][a-z0-9_]{0,55}$'
 ) THEN RAISE EXCEPTION 'feature_renamed: corpus contains an unknown flat feature; migrate it explicitly'; END IF;
 IF EXISTS (
   SELECT c.id, COALESCE(r.new_name,e.key) FROM corpus_examples c
   CROSS JOIN LATERAL jsonb_each(COALESCE(NULLIF(c.features,'null'::jsonb),'{}'::jsonb)) e
   LEFT JOIN abusekit_feature_rename r ON r.old_name=e.key
   GROUP BY c.id,COALESCE(r.new_name,e.key) HAVING count(*)>1
 ) THEN RAISE EXCEPTION 'feature_renamed: corpus contains colliding old and new keys'; END IF;
END $$;
UPDATE corpus_examples c SET features = COALESCE((
 SELECT jsonb_object_agg(COALESCE(r.new_name,e.key),e.value)
 FROM jsonb_each(COALESCE(NULLIF(c.features,'null'::jsonb),'{}'::jsonb)) e
 LEFT JOIN abusekit_feature_rename r ON r.old_name=e.key
),'{}'::jsonb), feature_key_space='ns-v1';
