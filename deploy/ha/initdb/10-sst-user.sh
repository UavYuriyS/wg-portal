#!/bin/bash
# Galera state transfer user (wsrep_sst_method=mariabackup). Replicated to all members with the first SST.
# Uses the mariadb client directly, so it works whether the entrypoint executes or sources this file.
set -eu
mariadb --protocol=socket -uroot -p"${MARIADB_ROOT_PASSWORD}" <<SQL
CREATE USER IF NOT EXISTS 'mariabackup'@'localhost' IDENTIFIED BY '${SST_PASSWORD}';
GRANT RELOAD, PROCESS, LOCK TABLES, BINLOG MONITOR ON *.* TO 'mariabackup'@'localhost';
SQL
