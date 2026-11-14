-- ============================================================================
-- dbtool 元数据库基础数据（002）。本文件直接维护（没有生成脚本）。
--
-- 写入的表：dbt_content_type、dbt_auth_permission、dbt_auth_group、dbt_auth_group_permissions、dbt_sql_instance_tag。
-- 来源说明：内容类型、权限（只保留代码里有判断点的权限码）、内置权限组与基线授权、实例标签；ID 固定（权限 ID 因此不连续）。
-- 生成日期：2026-10-13
-- 内容：内容类型 5 个、权限 57 个、内置权限组 5 个、实例标签 4 个。
-- 2026-10-23 精简：原先的 291 个权限里 233 个没有任何判断点（Django 自带模型权限、各模型的 add / change / delete / view、
--   已下线功能的菜单码），去掉；留下的 ID 不变。老库里这些行不受影响（本文件只用于空库）。
-- 2026-10-24（1.81）：再去掉 archive_review（没有对应按钮，迁移时本来就丢弃）。
-- 这里的权限码授权是【迁移的输入】：权限本身已经只认按钮，空库装好后由 `dbt-admin perm migrate --apply --yes`
--   （install.sh 在健康检查之后自动执行）把每个权限组持有的权限码展开成按钮授权，写进 dbt_group_perm。
--
-- 只用于【空库】初始化；全部 INSERT 都是幂等形式（重复键时空操作），重复执行不改变已有行。
-- 目标表都是带 dbt_ 前缀的基础表（见 001），不会碰同库里其它系统的表。
-- 这里的 ID 是固定值，不要在已经有其它权限数据的库上执行。
-- 不含任何用户、密码、密钥、实例信息；不写 dbt_setting / dbt_workflow_audit_setting / dbt_resource_group。
-- 执行顺序：001_base_schema.sql -> 本文件 -> deploy/sql/*.sql（其中 150_builtin_groups.sql 依赖这里的权限组）。
-- ============================================================================

SET NAMES utf8mb4;

-- ---- 1. dbt_content_type（5 行：留下的权限所属的内容类型）----
INSERT INTO `dbt_content_type` (`id`, `app_label`, `model`) VALUES
  (18, 'sql', 'instance'),
  (20, 'sql', 'permission'),
  (31, 'sql', 'paramtemplate'),
  (41, 'sql', 'partmanconfig'),
  (54, 'sql_perf', 'perfissue')
ON DUPLICATE KEY UPDATE `id` = `id`;

-- ---- 2. dbt_auth_permission（58 行：代码里有判断点的权限码）----
INSERT INTO `dbt_auth_permission` (`id`, `name`, `content_type_id`, `codename`) VALUES
  (69, 'Can add 实例配置', 18, 'add_instance'),
  (70, 'Can change 实例配置', 18, 'change_instance'),
  (71, 'Can delete 实例配置', 18, 'delete_instance'),
  (83, '菜单 SQL上线', 20, 'menu_sqlworkflow'),
  (84, '菜单 SQL分析', 20, 'menu_sqlanalyze'),
  (85, '菜单 SQL查询', 20, 'menu_query'),
  (87, '菜单 权限管理', 20, 'menu_queryapplylist'),
  (89, '菜单 优化工具', 20, 'menu_sqladvisor'),
  (90, '菜单 慢查日志', 20, 'menu_slowquery'),
  (92, '菜单 实例列表', 20, 'menu_instance_list'),
  (93, '菜单 会话管理', 20, 'menu_dbdiagnostic'),
  (94, '菜单 数据库管理', 20, 'menu_database'),
  (95, '菜单 实例账号管理', 20, 'menu_instance_account'),
  (96, '菜单 参数配置', 20, 'menu_param'),
  (99, '菜单 数据归档', 20, 'menu_archive'),
  (100, '菜单 My2SQL', 20, 'menu_my2sql'),
  (105, '提交SQL上线工单', 20, 'sql_submit'),
  (106, '审核SQL上线工单', 20, 'sql_review'),
  (107, '执行SQL上线工单(资源组粒度)', 20, 'sql_execute_for_resource_group'),
  (108, '执行SQL上线工单(仅自己提交的)', 20, 'sql_execute'),
  (109, '执行SQL分析', 20, 'sql_analyze'),
  (111, '执行SQLTuning', 20, 'optimize_sqltuning'),
  (112, '执行SOAR', 20, 'optimize_soar'),
  (113, '申请查询权限', 20, 'query_applypriv'),
  (114, '管理查询权限', 20, 'query_mgtpriv'),
  (115, '审核查询权限', 20, 'query_review'),
  (116, '提交SQL查询', 20, 'query_submit'),
  (117, '可查询所有实例', 20, 'query_all_instances'),
  (118, '可查询所在资源组内的所有实例', 20, 'query_resource_group_instance'),
  (119, '查看会话', 20, 'process_view'),
  (120, '终止会话', 20, 'process_kill'),
  (121, '查看表空间', 20, 'tablespace_view'),
  (122, '查看事务信息', 20, 'trx_view'),
  (123, '查看锁信息', 20, 'trxandlocks_view'),
  (124, '管理实例账号', 20, 'instance_account_manage'),
  (125, '查看实例参数列表', 20, 'param_view'),
  (126, '修改实例参数', 20, 'param_edit'),
  (128, '提交归档申请', 20, 'archive_apply'),
  (130, '管理归档申请', 20, 'archive_mgt'),
  (131, '审计权限', 20, 'audit_user'),
  (173, 'Can add 实例参数模板配置', 31, 'add_paramtemplate'),
  (174, 'Can change 实例参数模板配置', 31, 'change_paramtemplate'),
  (213, '菜单 分区维护', 20, 'menu_partman_list'),
  (214, '菜单 数据导出', 20, 'menu_sqlexportworkflow'),
  (215, '离线下载权限', 20, 'offline_download'),
  (216, '提交数据导出', 20, 'sqlexport_submit'),
  (245, '菜单 批量导出', 20, 'menu_sqlexportbatchworkflow'),
  (246, '提交批量导出', 20, 'sqlexportbatch_submit'),
  (248, '提交批量工单', 20, 'sqlbatch_submit'),
  (252, '菜单 性能巡检', 20, 'menu_mssqlinspection'),
  (261, 'Can add 分区维护配置', 41, 'add_partmanconfig'),
  (262, 'Can change 分区维护配置', 41, 'change_partmanconfig'),
  (263, 'Can delete 分区维护配置', 41, 'delete_partmanconfig'),
  (269, '管理定时巡检计划', 20, 'inspection_plan_manage'),
  (270, '清理实例Binlog', 20, 'binlog_del'),
  (311, '菜单 SQL性能工单', 54, 'menu_perf'),
  (312, '性能工单管理(DBA:建单/指派/验证/审批)', 54, 'perf_manage')
ON DUPLICATE KEY UPDATE `id` = `id`;

-- ---- 3. dbt_auth_group：dbtool 的内置权限组（名称与 deploy/sql/150_builtin_groups.sql 一致）----
INSERT INTO `dbt_auth_group` (`id`, `name`) VALUES
  (1, 'Default'),
  (2, 'RD'),
  (3, 'DBA'),
  (4, 'TL'),
  (5, 'QA')
ON DUPLICATE KEY UPDATE `id` = `id`;

-- ---- 4. dbt_auth_group_permissions：权限码授权 ----
-- 每组 = 基线授权 + 为迁移补充的权限码（迁移把每个码展开成它对应的全部按钮）。
-- 以 (group_id, permission_id) 唯一键去重；不指定行 ID，重复执行不改变已有行。
-- Default（id=1）：基线 3 个+ 补充 2 个 = 5 个
--   + query_applypriv  <- qpriv.list
--   + query_submit  <- query.run
INSERT INTO `dbt_auth_group_permissions` (`group_id`, `permission_id`) VALUES
  (1, 83),
  (1, 85),
  (1, 87),
  (1, 113),
  (1, 116)
ON DUPLICATE KEY UPDATE `group_id` = `group_id`;

-- RD（id=2）：基线 14 个+ 补充 4 个 = 18 个
--   + menu_partman_list  <- partman.view
--   + menu_sqlexportworkflow  <- export.view
--   + sqlexport_submit  <- export.submit
--   + menu_perf  <- perf.view / perf.handle / perf.comment（sql_perf 应用）
INSERT INTO `dbt_auth_group_permissions` (`group_id`, `permission_id`) VALUES
  (2, 83),
  (2, 84),
  (2, 85),
  (2, 87),
  (2, 89),
  (2, 90),
  (2, 99),
  (2, 105),
  (2, 108),
  (2, 109),
  (2, 112),
  (2, 113),
  (2, 116),
  (2, 128),
  (2, 213),
  (2, 214),
  (2, 216),
  (2, 311)
ON DUPLICATE KEY UPDATE `group_id` = `group_id`;

-- DBA（id=3）：基线 36 个+ 补充 20 个 = 56 个
--   + add_instance  <- instance.add / instance.test
--   + change_instance  <- instance.edit / instance.test
--   + delete_instance  <- instance.delete
--   + add_paramtemplate  <- param.template
--   + change_paramtemplate  <- param.template
--   + menu_partman_list  <- partman.view
--   + menu_sqlexportworkflow  <- export.view
--   + offline_download  <- export.download
--   + sqlexport_submit  <- export.submit
--   + menu_sqlexportbatchworkflow  <- export.view_batch
--   + sqlexportbatch_submit  <- export.submit_batch
--   + sqlbatch_submit  <- sqlworkflow.submit_batch
--   + menu_mssqlinspection  <- inspect.view
--   + add_partmanconfig  <- partman.add
--   + change_partmanconfig  <- partman.edit / partman.run
--   + delete_partmanconfig  <- partman.delete
--   + inspection_plan_manage  <- inspect.plan_edit / plan_toggle / plan_run（dbtool 新增权限码）
--   + binlog_del  <- instance.purge_binlog（dbtool 新增权限码）
--   + menu_perf  <- perf.view（sql_perf 应用）
--   + perf_manage  <- perf.create / assign / verify / approve / adjust / reopen / close_dup、perfcfg.*（sql_perf 应用）
INSERT INTO `dbt_auth_group_permissions` (`group_id`, `permission_id`) VALUES
  (3, 69),
  (3, 70),
  (3, 71),
  (3, 83),
  (3, 84),
  (3, 85),
  (3, 87),
  (3, 89),
  (3, 90),
  (3, 92),
  (3, 93),
  (3, 94),
  (3, 95),
  (3, 96),
  (3, 99),
  (3, 100),
  (3, 105),
  (3, 106),
  (3, 107),
  (3, 108),
  (3, 109),
  (3, 111),
  (3, 112),
  (3, 113),
  (3, 114),
  (3, 115),
  (3, 116),
  (3, 117),
  (3, 118),
  (3, 119),
  (3, 120),
  (3, 121),
  (3, 122),
  (3, 123),
  (3, 124),
  (3, 125),
  (3, 126),
  (3, 128),
  (3, 130),
  (3, 173),
  (3, 174),
  (3, 213),
  (3, 214),
  (3, 215),
  (3, 216),
  (3, 245),
  (3, 246),
  (3, 248),
  (3, 252),
  (3, 261),
  (3, 262),
  (3, 263),
  (3, 269),
  (3, 270),
  (3, 311),
  (3, 312)
ON DUPLICATE KEY UPDATE `group_id` = `group_id`;

-- TL（id=4）：基线 17 个+ 补充 7 个 = 24 个
--   + menu_instance_list  <- instance.view
--   + query_resource_group_instance  <- query.group_run
--   + archive_mgt  <- archive.pause（注意：迁移时同一个码还会展开成 archive.run / stop / restore / delete）
--   + menu_partman_list  <- partman.view
--   + menu_sqlexportworkflow  <- export.view
--   + sqlexport_submit  <- export.submit
--   + menu_perf  <- perf.view / perf.handle / perf.comment（sql_perf 应用）
INSERT INTO `dbt_auth_group_permissions` (`group_id`, `permission_id`) VALUES
  (4, 83),
  (4, 84),
  (4, 85),
  (4, 87),
  (4, 89),
  (4, 90),
  (4, 92),
  (4, 99),
  (4, 105),
  (4, 106),
  (4, 107),
  (4, 108),
  (4, 109),
  (4, 112),
  (4, 113),
  (4, 115),
  (4, 116),
  (4, 118),
  (4, 128),
  (4, 130),
  (4, 213),
  (4, 214),
  (4, 216),
  (4, 311)
ON DUPLICATE KEY UPDATE `group_id` = `group_id`;

-- QA（id=5）：基线 7 个+ 补充 6 个 = 13 个
--   + menu_sqladvisor  <- optimize.explain
--   + menu_slowquery  <- slowlogs.view
--   + menu_archive  <- archive.view
--   + menu_sqlexportworkflow  <- export.view
--   + offline_download  <- export.download
--   + sqlexport_submit  <- export.submit
INSERT INTO `dbt_auth_group_permissions` (`group_id`, `permission_id`) VALUES
  (5, 83),
  (5, 85),
  (5, 87),
  (5, 89),
  (5, 90),
  (5, 99),
  (5, 105),
  (5, 108),
  (5, 113),
  (5, 116),
  (5, 214),
  (5, 215),
  (5, 216)
ON DUPLICATE KEY UPDATE `group_id` = `group_id`;

-- ---- 5. dbt_sql_instance_tag：dbtool 逻辑依赖的实例标签 ----
-- can_read：权限申请只列出带该标签的实例；
-- can_write：可写实例；mysql_inspection / mssql_inspection：定时巡检按标签圈定实例。
-- 标签 ID 固定，3 号空缺。
INSERT INTO `dbt_sql_instance_tag` (`id`, `tag_code`, `tag_name`, `active`, `create_time`) VALUES
  (1, 'can_write', '可以写入', 1, NOW(6)),
  (2, 'can_read', '支持查询', 1, NOW(6)),
  (4, 'mysql_inspection', 'MySQL巡检', 1, NOW(6)),
  (5, 'mssql_inspection', 'MSSQL巡检', 1, NOW(6))
ON DUPLICATE KEY UPDATE `id` = `id`;
