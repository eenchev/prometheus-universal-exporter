package config

import (
	"errors"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// The Python of a configuration's collectors is checked with the interpreter
// once the configuration has loaded (transform.CheckPythonScripts): a syntax
// error, a pre_script that never produces data. A problem with a collector
// that a collector file defines names that file, as a validation error of
// that collector does (inCollectorFile), at startup, in --dry-run and on
// reload: that file, not the configuration listing it, is the one to edit.
// The check gives each problem with the name of its collector, and the file
// is the one that collector was read from, so the interpreter runs once
// however many collector files there are and whatever it finds.

// ValidatePythonScripts refuses a configuration whose Python cannot work,
// reporting every problem (model.Problems), each naming the collector file
// its collector is defined in. A configuration without Python needs no
// interpreter.
func ValidatePythonScripts(pythonPath string, c *model.Config) error {
	problems, err := CheckPythonScripts(pythonPath, c)
	if err != nil {
		return err
	}
	return model.JoinProblems(problems...)
}

// CheckPythonScripts is ValidatePythonScripts with the faults kept apart: err
// is the interpreter itself failing, and problems are the faults of the
// scripts, in the order of the collectors, which --dry-run reports one by
// one.
func CheckPythonScripts(pythonPath string, c *model.Config) (problems []error, err error) {
	found, err := transform.CheckPythonScripts(pythonPath, c)
	if err != nil || len(found) == 0 {
		return nil, err
	}
	for _, problem := range found {
		problems = append(problems, inCollectorFile(collectorFileOf(c, problem.Collector), errors.New(problem.Message)))
	}
	return problems, nil
}
