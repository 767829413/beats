// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package filters

import (
	"fmt"
	"runtime"

	"github.com/Masterminds/semver"

	"github.com/elastic/beats/v7/x-pack/agent/pkg/agent/application/info"
	"github.com/elastic/beats/v7/x-pack/agent/pkg/agent/errors"
	"github.com/elastic/beats/v7/x-pack/agent/pkg/agent/transpiler"
	"github.com/elastic/beats/v7/x-pack/agent/pkg/boolexp"
	"github.com/elastic/beats/v7/x-pack/agent/pkg/core/logger"
	"github.com/elastic/beats/v7/x-pack/agent/pkg/release"
	"github.com/elastic/go-sysinfo"
)

const (
	datasourcesKey          = "datasources"
	constraintsKey          = "constraints"
	validateVersionFuncName = "validate_version"
)

// List of variables available to be used in constraint definitions.
const (
	// `agent.id` is a generated (in standalone) or assigned (in fleet) agent identifier.
	agentIDKey = "agent.id"
	// `agent.version` specifies current version of an agent.
	agentVersionKey = "agent.version"
	// `host.architecture` defines architecture of a host (e.g. x86_64, arm, ppc, mips).
	hostArchKey = "host.architecture"
	// `os.family` defines a family of underlying operating system (e.g. redhat, debian, freebsd, windows).
	osFamilyKey = "os.family"
	// `os.kernel` specifies current version of a kernel in a semver format.
	osKernelKey = "os.kernel"
	// `os.platform` specifies platform agent is running on (e.g. centos, ubuntu, windows).
	osPlatformKey = "os.platform"
	// `os.version` specifies version of underlying operating system (e.g. 10.12.6).
	osVersionKey = "os.version"
)

var (
	boolexpVarStore    *constraintVarStore
	boolexpMethodsRegs *boolexp.MethodsReg
)

// ConstraintFilter filters ast based on included constraints.
func ConstraintFilter(log *logger.Logger, ast *transpiler.AST) error {
	// get datasources
	dsNode, found := transpiler.Lookup(ast, datasourcesKey)
	if !found {
		return nil
	}

	dsListNode, ok := dsNode.Value().(*transpiler.List)
	if !ok {
		return nil
	}

	dsList, ok := dsListNode.Value().([]transpiler.Node)
	if !ok {
		return nil
	}

	// for each datasource
	i := 0
	originalLen := len(dsList)
	for i < len(dsList) {
		constraintMatch, err := evaluateConstraints(log, dsList[i])
		if err != nil {
			return err
		}

		if constraintMatch {
			i++
			continue
		}
		dsList = append(dsList[:i], dsList[i+1:]...)
	}

	if len(dsList) == originalLen {
		return nil
	}

	// Replace datasources with limited set
	if err := transpiler.RemoveKey(datasourcesKey).Apply(ast); err != nil {
		return err
	}

	newList := transpiler.NewList(dsList)
	return transpiler.Insert(ast, newList, datasourcesKey)
}

func evaluateConstraints(log *logger.Logger, datasourceNode transpiler.Node) (bool, error) {
	constraintsNode, found := datasourceNode.Find(constraintsKey)
	if !found {
		return true, nil
	}

	constraintsListNode, ok := constraintsNode.Value().(*transpiler.List)
	if !ok {
		return false, errors.New("constraints not a list", errors.TypeConfig)
	}

	constraintsList, ok := constraintsListNode.Value().([]transpiler.Node)
	if !ok {
		return false, errors.New("constraints not a list", errors.TypeConfig)
	}

	for _, c := range constraintsList {
		strval, ok := c.(*transpiler.StrVal)
		if !ok {
			return false, errors.New("constraints is not a string")
		}

		constraint := strval.String()
		if isOK, err := evaluateConstraint(constraint); !isOK || err != nil {
			if err == nil {
				// log only constraint not matching
				log.Infof("constraint '%s' not matching for datasource '%s'", constraint, datasourceIdentifier(datasourceNode))
			}

			return false, err
		}
	}

	return true, nil
}

func datasourceIdentifier(datasourceNode transpiler.Node) string {
	namespace := "default"
	output := "default"

	if nsNode, found := datasourceNode.Find("namespace"); found {
		nsKey, ok := nsNode.(*transpiler.Key)
		if ok {
			if valNode, ok := nsKey.Value().(transpiler.Node); ok {
				namespace = valNode.String()
			}
		}
	}

	if outNode, found := datasourceNode.Find("use_output"); found {
		nsKey, ok := outNode.(*transpiler.Key)
		if ok {
			if valNode, ok := nsKey.Value().(transpiler.Node); ok {
				output = valNode.String()
			}
		}
	}

	ID := "unknown"
	if idNode, found := datasourceNode.Find("id"); found {
		nsKey, ok := idNode.(*transpiler.Key)
		if ok {
			if valNode, ok := nsKey.Value().(transpiler.Node); ok {
				ID = valNode.String()
			}
		}
	}

	return fmt.Sprintf("namespace:%s, output:%s, id:%s", namespace, output, ID)
}

func evaluateConstraint(constraint string) (bool, error) {
	store, regs, err := boolexpMachinery()
	if err != nil {
		return false, err
	}

	return boolexp.Eval(constraint, regs, store)
}

func boolexpMachinery() (*constraintVarStore, *boolexp.MethodsReg, error) {
	if boolexpMethodsRegs != nil && boolexpVarStore != nil {
		return boolexpVarStore, boolexpMethodsRegs, nil
	}

	regs := boolexp.NewMethodsReg()
	if err := regs.Register(validateVersionFuncName, regValidateVersion); err != nil {
		return nil, nil, err
	}

	store, err := newVarStore()
	if err != nil {
		return nil, nil, err
	}

	if err := initVarStore(store); err != nil {
		return nil, nil, err
	}

	boolexpMethodsRegs = regs
	boolexpVarStore = store

	return boolexpVarStore, boolexpMethodsRegs, nil
}

func regValidateVersion(args []interface{}) (interface{}, error) {
	if len(args) != 2 {
		return false, errors.New("validate_version: invalid number of arguments, expecting 2")
	}

	version, isString := args[0].(string)
	if !isString {
		return false, errors.New("version should be a string")
	}

	constraint, isString := args[1].(string)
	if !isString {
		return false, errors.New("version constraint should be a string")
	}

	c, err := semver.NewConstraint(constraint)
	if err != nil {
		return false, errors.New(fmt.Sprintf("constraint '%s' is invalid", constraint))
	}

	v, err := semver.NewVersion(version)
	if err != nil {
		return false, errors.New(fmt.Sprintf("version '%s' is invalid", version))
	}

	isOK, _ := c.Validate(v)
	return isOK, nil
}

type constraintVarStore struct {
	vars map[string]interface{}
}

func (s *constraintVarStore) Lookup(v string) (interface{}, bool) {
	val, ok := s.vars[v]
	return val, ok
}

func newVarStore() (*constraintVarStore, error) {
	return &constraintVarStore{
		vars: make(map[string]interface{}),
	}, nil
}

func initVarStore(store *constraintVarStore) error {
	sysInfo, err := sysinfo.Host()
	if err != nil {
		return err
	}

	agentInfo, err := info.NewAgentInfo()
	if err != nil {
		return err
	}

	info := sysInfo.Info()

	// 	Agent
	store.vars[agentIDKey] = agentInfo.AgentID()
	store.vars[agentVersionKey] = release.Version()

	// Host
	store.vars[hostArchKey] = info.Architecture

	// Operating system
	store.vars[osFamilyKey] = runtime.GOOS
	store.vars[osKernelKey] = info.KernelVersion
	store.vars[osPlatformKey] = info.OS.Family
	store.vars[osVersionKey] = info.OS.Version

	return nil
}
