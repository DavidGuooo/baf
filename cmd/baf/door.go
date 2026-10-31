package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"
)

func doorCommand(args []string) error {
	usage := errors.New("usage: baf door list [--json] | baf door revoke VENDOR/NAME")
	if len(args) == 0 {
		return usage
	}
	fs := flag.NewFlagSet("door", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	if (args[0] != "list" && args[0] != "revoke") || (args[0] == "list" && len(pos) != 0) || (args[0] == "revoke" && (len(pos) != 1 || *asJSON)) {
		return usage
	}
	c, err := client()
	if err != nil {
		return err
	}
	if args[0] == "revoke" {
		if err := c.DoorRevoke(pos[0]); err != nil {
			return err
		}
		fmt.Printf("revoked connector for %s\n", pos[0])
		return nil
	}
	list, err := c.DoorList()
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(list)
	}
	if len(list) == 0 {
		fmt.Println("the policy names no Door agents on this Relay")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "DOOR AGENT\tCONNECTOR\tAPPROVED BY\tAPPROVED AT\tCLIENT ID")
	for _, c := range list {
		approved := "-"
		if !c.ApprovedAt.IsZero() {
			approved = c.ApprovedAt.Local().Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", c.Agent, c.State, c.ApprovedBy, approved, c.ClientID)
	}
	return tw.Flush()
}
