-- Let the app user read server status and statement digests (Top SQL for lt).
GRANT PROCESS, REPLICATION CLIENT ON *.* TO 'lt'@'%';
GRANT SELECT ON performance_schema.* TO 'lt'@'%';
