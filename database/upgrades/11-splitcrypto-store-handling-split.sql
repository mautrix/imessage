-- v11: Move crypto/state store upgrade handling to separate systems
CREATE TABLE crypto_version (version INTEGER PRIMARY KEY);
INSERT INTO crypto_version VALUES (6);
CREATE TABLE mx_version (version INTEGER PRIMARY KEY);
INSERT INTO mx_version VALUES (3);
