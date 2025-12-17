package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/aojea/krun/cmd/jobset"
	"github.com/aojea/krun/cmd/run"

	"k8s.io/klog/v2"
)

var rootCmd = &cobra.Command{
	Use:   "krun",
	Short: "krun is a tool to simplify AI/ML workflows on Kubernetes",
}

func main() {
	klog.InitFlags(nil)
	// only add the -v flag to the root command
	rootCmd.PersistentFlags().AddGoFlag(flag.CommandLine.Lookup("v"))

	// run works on Pods selected by label
	rootCmd.AddCommand(run.RunCmd)
	// jobset works on Pods belonging to a JobSet
	rootCmd.AddCommand(jobset.JobSetCmd)


	ctx, cancel := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		klog.Info(err)
		os.Exit(1)
	}

}
