// @author: kevinkelly
// @license: Apache Licence
// @file: privapply_groupinst.go
// @time: 2026/10/28

package store

import "dbtool/internal/privapply"

var _ privapply.GroupInstanceRepo = (*Store)(nil)

// GroupInstanceIDs 实现 privapply.GroupInstanceRepo：返回资源组关联的实例（dbt_sql_instance_resource_group，只读）。
func (s *Store) GroupInstanceIDs(groupID uint) (map[uint]bool, error) {
	var ids []uint
	if err := s.DB.Table("dbt_sql_instance_resource_group").Where("resourcegroup_id = ?", groupID).Pluck("instance_id", &ids).Error; err != nil {
		return nil, err
	}
	out := make(map[uint]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}
