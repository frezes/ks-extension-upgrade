package main

import (
	"context"
	"flag"
	"os"
	"time"

	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/kubesphere-extensions/upgrade/pkg/core"
)

func init() {
	fs := flag.NewFlagSet("", flag.ExitOnError)
	config.RegisterFlags(fs)
}

func main() {
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	coreHelper, err := core.NewCoreHelper()
	if err != nil {
		klog.Errorf("failed to create coreHelper: %s", err)
		return
	}

	if err = coreHelper.Run(ctx); err != nil {
		os.Exit(1)
	}
}
