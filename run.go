package main

import (
	"context"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/pokt-network/pocket-runner/internal/types"
	"github.com/pokt-network/pocket-runner/x/runner"
	tmTypes "github.com/tendermint/tendermint/types"
)

func Run(args []string) {
	cfg, err := types.GetConfigFromEnv()
	if err != nil {
		log.Printf("%+v\n", err)
		os.Exit(1)
	}
	// Initial launcher, separated from loop due to passphrase
	cmd, err := runner.LaunchProcess(cfg, args, os.Stdout, os.Stderr, os.Stdin)
	if err != nil {
		log.Printf("%+v\n", err)
		os.Exit(1)
	}
	time.Sleep(time.Second * 10)

	errors := make(chan error)
	upgrades := make(chan *types.UpgradeInfo)
	commands := make(chan *exec.Cmd)
	var tmListener = runner.NewEventListener(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	log.Println("starting listeners")

	signals := make(chan os.Signal, 1)
	signal.Notify(signals,
		syscall.SIGTERM,
		syscall.SIGINT,
		syscall.SIGQUIT,
		os.Kill,
		os.Interrupt)

	fanJobs := func(ctx context.Context, cfg *types.Config, args []string, cmd *exec.Cmd, listener *runner.EventListener, upgrades chan *types.UpgradeInfo, commands chan *exec.Cmd, errors chan error) {
		go WaitForUpgrade(ctx, cfg, tmListener, upgrades, errors)
		go WaitForBlockHeight(ctx, cfg, args, cmd, tmListener, upgrades, commands, errors)
	}

	go func() {
		fanJobs(ctx, cfg, args, cmd, tmListener, upgrades, commands, errors)
		for {
			select {
			case currentCommand := <-commands:
				cmd, err = runner.LaunchProcess(cfg, args, os.Stdout, os.Stderr, os.Stdin)
				if err != nil {
					errors <- err
				}
				cancel()
				time.Sleep(time.Second * 5)
				ctx, cancel = context.WithCancel(context.Background())
				tmListener = tmListener.Reset(cfg)
				fanJobs(ctx, cfg, args, currentCommand, tmListener, upgrades, commands, errors)
			}
		}
	}()

	log.Println("Loop is begining!")
	for {
		select {
		case err := <-errors:
			log.Printf("%+v\n", err)
			os.Exit(1)
		case <-signals:
			cancel()
			tmListener.Stop()
			if err := cmd.Process.Kill(); err != nil {
				log.Printf("%+v\n", err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
}

// WaitForBlockHeight listens for upgrades, per upgrade checks the current block header & upgrades if neccesary.
func WaitForBlockHeight(ctx context.Context, cfg *types.Config, args []string, cmd *exec.Cmd, listener *runner.EventListener, upgrades chan *types.UpgradeInfo, commands chan *exec.Cmd, errors chan error) {
	log.Printf("\n *****Listen For BlockHeight***** \n")
	var err error
	var currentUpgrade *types.UpgradeInfo

	for {
		select {
		case rawHeaderEvt := <-listener.HeaderChan:
			if currentUpgrade == nil {
				// wait for upgrade if no current upgrade this way the blockHeight won't change
				currentUpgrade = <-upgrades
			}
			upgrade := currentUpgrade
			headerEvt := rawHeaderEvt.Data.(tmTypes.EventDataNewBlockHeader)
			log.Printf("\n *****Received Block Header for Height %v ***** \n", headerEvt.Header.Height)
			if upgrade.Height != headerEvt.Header.Height {
				continue
			}
			if err := cmd.Process.Kill(); err != nil { // PROCESS MUST DIE BEFORE UPGRADING; cfg.Current is a symlink otherwise bugs might happen
				errors <- err
			}
			if err := runner.Upgrade(cfg, upgrade); err != nil {
				errors <- err
			}
			log.Printf("Upgrade to %s performed successfully!!\n", upgrade.Name)
			cmd, err = runner.LaunchProcess(cfg, args, os.Stdout, os.Stderr, os.Stdin)
			if err != nil {
				errors <- err
			}
			commands <- cmd
		case <-ctx.Done():
			return
		}
	}
}

// WaitForUpgrade listens transactions and filters upgrades, passess them to the upgrade channel
func WaitForUpgrade(ctx context.Context, cfg *types.Config, listener *runner.EventListener, upgrades chan *types.UpgradeInfo, errors chan error) {
	log.Printf("\n *****Wait for Upgrade***** \n")
	for {
		upgrade := &types.UpgradeInfo{}
		select {
		case rawTxEvt := <-listener.TxChan:
			log.Printf("\n *****Received a Tx***** \n")
			if len(rawTxEvt.Events["upgrade.action"]) == 1 {
				log.Printf("\n *****Received an Upgrade***** \n")
				if err := upgrade.SetUpgrade(strings.Join(rawTxEvt.Events["upgrade.action"], "")); err != nil {
					errors <- err
				}

				if err := types.CheckBinary(cfg.UpgradeBin(upgrade.Name)); err != nil {
					if cfg.AllowDownload {
						if er := runner.DownloadBinary(cfg, upgrade); er != nil {
							errors <- er
						} else {
							upgrades <- upgrade
							continue
						}
					} else {
						errors <- err
					}
				}
				log.Printf("\n *****Sent an Upgrade***** \n")
				upgrades <- upgrade
			}
		case <-ctx.Done():
			return // singal to kill process was sent terminate exectuion
		}
	}
}
