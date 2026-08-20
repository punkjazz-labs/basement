package main

import "testing"

func TestMacOSLaunchDetachesTheWizardWorker(t *testing.T) {
	if !shouldLaunchWizardWorker("darwin", nil) {
		t.Fatal("an initial macOS app launch kept the long-lived wizard inside the Launch Services process")
	}
	if shouldLaunchWizardWorker("darwin", []string{wizardWorkerArg}) {
		t.Fatal("the detached macOS worker tried to launch another worker")
	}
	for _, goos := range []string{"windows", "linux"} {
		if shouldLaunchWizardWorker(goos, nil) {
			t.Errorf("%s unexpectedly took the macOS launcher path", goos)
		}
	}
}
