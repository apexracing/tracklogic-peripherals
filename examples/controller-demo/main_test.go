package main

import (
	"testing"

	"github.com/apexracing/tracklogic-peripherals/pkg/controller"
)

func TestValidateDistinctPedalBindings(t *testing.T) {
	results := []pedalTestResult{
		{logicalName: "Brake", binding: controller.AxisBinding{DeviceInstanceGUID: "pedals", Axis: 1}},
		{logicalName: "Throttle", binding: controller.AxisBinding{DeviceInstanceGUID: "pedals", Axis: 2}},
		{logicalName: "Clutch", binding: controller.AxisBinding{DeviceInstanceGUID: "wheel", Axis: 1}},
	}
	if err := validateDistinctPedalBindings(results); err != nil {
		t.Fatalf("distinct bindings rejected: %v", err)
	}
}

func TestValidateDistinctPedalBindingsRejectsDuplicate(t *testing.T) {
	results := []pedalTestResult{
		{logicalName: "Brake", binding: controller.AxisBinding{DeviceInstanceGUID: "pedals", Axis: 1}},
		{logicalName: "Throttle", binding: controller.AxisBinding{DeviceInstanceGUID: "pedals", Axis: 1}},
	}
	if err := validateDistinctPedalBindings(results); err == nil {
		t.Fatal("duplicate pedal bindings were accepted")
	}
}
