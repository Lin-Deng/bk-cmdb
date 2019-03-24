/*
 * Tencent is pleased to support the open source community by making 蓝鲸 available.
 * Copyright (C) 2017-2018 THL A29 Limited, a Tencent company. All rights reserved.
 * Licensed under the MIT License (the "License"); you may not use this file except
 * in compliance with the License. You may obtain a copy of the License at
 * http://opensource.org/licenses/MIT
 * Unless required by applicable law or agreed to in writing, software distributed under
 * the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
 * either express or implied. See the License for the specific language governing permissions and
 * limitations under the License.
 */

package handler

import (
	"context"
	"fmt"
	"strings"

	"configcenter/src/auth/authcenter"
	"configcenter/src/auth/extensions"
	authmeta "configcenter/src/auth/meta"
	"configcenter/src/common"
	"configcenter/src/common/blog"
	"configcenter/src/common/condition"
	"configcenter/src/common/metadata"
	"configcenter/src/scene_server/admin_server/synchronizer/meta"
	"configcenter/src/scene_server/admin_server/synchronizer/utils"
)

// HandleModuleSync do sync module of one business
func (ih *IAMHandler) HandleModuleSync(task *meta.WorkRequest) error {
	businessSimplify := task.Data.(extensions.BusinessSimplify)
	header := utils.NewAPIHeaderByBusiness(&businessSimplify)
	coreService := ih.CoreAPI.CoreService()

	// step1 get instances by business from core service
	cond := condition.CreateCondition()
	cond.Field(common.BKAppIDField).Eq(businessSimplify.BKAppIDField)
	query := &metadata.QueryCondition{
		Fields:    []string{common.BKAppIDField, common.BKModuleIDField, common.BKModuleNameField},
		Condition: cond.ToMapStr(),
		Limit:     metadata.SearchLimit{Limit: common.BKNoLimit},
	}
	instances, err := coreService.Instance().ReadInstance(context.Background(), *header, common.BKInnerObjIDModule, query)
	if err != nil {
		blog.Errorf("get module:%+v by businessID:%d failed, err: %+v", businessSimplify.BKAppIDField, err)
		return fmt.Errorf("get module by businessID:%d failed, err: %+v", businessSimplify.BKAppIDField, err)
	}

	if len(instances.Data.Info) == 0 {
		blog.V(2).Infof("business: %d has no instances, skip synchronize modules.", businessSimplify.BKAppIDField)
		return nil
	}

	// extract modules
	moduleArr := make([]extensions.ModuleSimplify, 0)
	for _, instance := range instances.Data.Info {
		moduleSimplify := extensions.ModuleSimplify{}
		_, err := moduleSimplify.Parse(instance)
		if err != nil {
			blog.Errorf("parse module: %+v simplify infomation failed, err: %+v", instance, err)
			continue
		}
		moduleArr = append(moduleArr, moduleSimplify)
	}

	blog.V(4).Infof("list modules by business:%d result: %+v", businessSimplify.BKAppIDField, moduleArr)

	// step2 get modules by business from iam
	rs := &authmeta.ResourceAttribute{
		Basic: authmeta.Basic{
			Type: authmeta.ModelModule,
		},
		SupplierAccount: "",
		BusinessID:      businessSimplify.BKAppIDField,
		Layers: make([]authmeta.Item, 0),
		// iam don't support module layers yet.
		// Layers:          []authmeta.Item{{Type: authmeta.Business, InstanceID: businessID,}},
	}
	resultResources, err := ih.Authorizer.ListResources(context.Background(), rs)
	if err != nil {
		blog.Errorf("synchronize module instance failed, ListResources failed, err: %+v", err)
		return err
	}
	// filter module from topo model instances
	realResources := make([]authmeta.BackendResource, 0)
	for _, iamResources := range resultResources {
		if strings.Contains(iamResources[len(iamResources)-1].ResourceID, "module") {
			realResources = append(realResources, iamResources)
		}
	}
	blog.InfoJSON("realResources is: %s", realResources)

	// init key:hit map for
	iamResourceKeyMap := map[string]int{}
	for _, iamResource := range realResources {
		key := generateIAMResourceKey(iamResource)
		// init hit count 0
		iamResourceKeyMap[key] = 0
	}

	// step6 register module not exist in iam
	// step5 diff step2 and step4 result
	scope := authcenter.ScopeInfo{}
	needRegister := make([]authmeta.ResourceAttribute, 0)
	for _, module := range moduleArr {
		resource := authmeta.ResourceAttribute{
			Basic: authmeta.Basic{
				Type:       authmeta.ModelModule,
				Name:       module.BKModuleNameField,
				InstanceID: module.BKModuleIDField,
			},
			SupplierAccount: "",
			BusinessID:      businessSimplify.BKAppIDField,
			// Layers:          layer[0:1],
	}
		targetResource, err := ih.Authorizer.DryRunRegisterResource(context.Background(), resource)
		if err != nil {
			blog.Errorf("synchronize module instance failed, dry run register resource failed, err: %+v", err)
			return err
		}
		if len(targetResource.Resources) != 1 {
			blog.Errorf("synchronize instance:%+v failed, dry run register result is: %+v", resource, targetResource)
			continue
		}
		scope.ScopeID = targetResource.Resources[0].ScopeID
		scope.ScopeType = targetResource.Resources[0].ScopeType
		resourceKey := generateCMDBResourceKey(&targetResource.Resources[0])
		_, exist := iamResourceKeyMap[resourceKey]
		if exist {
			iamResourceKeyMap[resourceKey]++
		} else {
			needRegister = append(needRegister, resource)
		}
	}
	blog.V(5).Infof("iamResourceKeyMap: %+v", iamResourceKeyMap)
	blog.V(5).Infof("needRegister: %+v", needRegister)
	if len(needRegister) > 0 {
		blog.V(2).Infof("sychronizer register resource that only in cmdb, resources: %+v", needRegister)
		err = ih.Authorizer.RegisterResource(context.Background(), needRegister...)
		if err != nil {
			blog.ErrorJSON("sychronizer register resource that only in cmdb failed, resources: %s, err: %+v", needRegister, err)
		}
	}

	// step7 deregister resource id that hasn't been hit
	needDeregister := make([]authmeta.BackendResource, 0)
	for _, iamResource := range realResources {
		resourceKey := generateIAMResourceKey(iamResource)
		if iamResourceKeyMap[resourceKey] == 0 {
			needDeregister = append(needDeregister, iamResource)
		}
	}
	blog.V(5).Infof("needDeregister: %+v", needDeregister)
	if len(needDeregister) != 0 {
		blog.V(2).Infof("sychronizer deregister resource that only in iam, resources: %+v", needDeregister)
		err = ih.Authorizer.RawDeregisterResource(context.Background(), scope, needDeregister...)
		if err != nil {
			blog.ErrorJSON("sychronizer deregister resource that only in iam failed, resources: %s, err: %+v", needDeregister, err)
		}
	}

	return nil
}
