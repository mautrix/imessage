-- v6: Split last_used into last_encrypted and last_decrypted in crypto store
ALTER TABLE crypto_olm_session RENAME COLUMN last_used TO last_decrypted;
ALTER TABLE crypto_olm_session ADD COLUMN last_encrypted timestamp;
UPDATE crypto_olm_session SET last_encrypted=last_decrypted;
