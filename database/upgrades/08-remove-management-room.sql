-- v8: Remove management room column from users

ALTER TABLE user DROP COLUMN management_room;
