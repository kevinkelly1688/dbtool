-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 280_partman_alert.sql
-- @time: 2026/10/28

-- 分区维护的“可用分区数量告警”（dbtool 新增）。需要在元数据库里执行一次（升级时 install.sh 会自动执行 sql/*.sql）；脚本可重复执行。
-- 不改 dbt_partman_config / dbt_partman_log（它们与外部程序共用，只读写既有列），告警用到的数据单独放在下面两张表里。
-- 表不存在时：“分区维护”页的告警设置提示先执行 280_partman_alert.sql，定时检查不推送，其它功能不受影响。

CREATE TABLE IF NOT EXISTS dbt_partman_alert (
  cnf_id        BIGINT   NOT NULL COMMENT '配置信息ID（dbt_partman_config.cnf_id）',
  threshold     INT      NULL COMMENT '本表的告警阈值（可用分区数小于它就告警）；NULL = 跟随系统配置 partition.alert_min_available，0 = 本表不告警',
  silence_until DATETIME NULL COMMENT '静默到何时（期间只在列表里标记，不推送）',
  alerting      TINYINT  NOT NULL DEFAULT 0 COMMENT '上一次检查时是否处于告警：1 是 0 否（用来去重，也用来判断“已恢复”）',
  last_alert_at DATETIME NULL COMMENT '上一次成功推送告警 / 重复提醒的时间',
  updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (cnf_id)
) ENGINE=InnoDB COMMENT='分区维护：每张表的可用分区告警阈值、静默与告警状态';

CREATE TABLE IF NOT EXISTS dbt_partman_alert_log (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  cnf_id     BIGINT       NOT NULL COMMENT '配置信息ID',
  kind       VARCHAR(10)  NOT NULL COMMENT 'alert 首次告警 / repeat 重复提醒 / recover 已恢复',
  available  INT          NOT NULL COMMENT '检查时的可用分区数',
  threshold  INT          NOT NULL COMMENT '检查时生效的阈值',
  channels   VARCHAR(200) NOT NULL DEFAULT '' COMMENT '推送的通道类型，逗号分隔',
  ok         TINYINT      NOT NULL COMMENT '至少一个通道推送成功：1 是 0 否',
  message    VARCHAR(500) NOT NULL DEFAULT '' COMMENT '推送失败的原因（地址已脱敏）',
  created_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY idx_cnf (cnf_id, created_at),
  KEY idx_created (created_at)
) ENGINE=InnoDB COMMENT='分区维护：可用分区告警的推送记录';
