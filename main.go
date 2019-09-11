package main

import (
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	"github.com/abiosoft/ishell"
	"github.com/hashicorp/hcl"
)

var (
	configFileLocation = flag.String("config", "glorious.glorious", "config file location")
)

func main() {
	flag.Parse()

	config, err := loadConfig(*configFileLocation)
	if err != nil {
		fmt.Println("failed to load config: ", err)
		os.Exit(1)
	}

	shell := ishell.New()
	shell.SetHomeHistoryPath(".glorious/history")

	// display welcome info.
	shell.Println(banner)

	// register a function for "greet" command.
	shell.AddCmd(&ishell.Cmd{
		Name: "greet",
		Help: "greet user",
		Func: func(c *ishell.Context) {
			c.Println("Hello", strings.Join(c.Args, " "))
		},
	})

	shell.AddCmd(&ishell.Cmd{
		Name: "config",
		Help: "Display config information",
		Func: func(c *ishell.Context) {
			c.Printf("%-20s| %-9s| Description\n", "Name", "# units")
			for _, unit := range config.Units {
				c.Printf("%-20s| %-9s| %s\n",
					unit.Name,
					fmt.Sprintf("%d units", len(unit.Slots)),
					unit.Description,
				)
			}

		},
	})

	shell.AddCmd(&ishell.Cmd{
		Name: "status",
		Help: "Display status information",
		Func: func(c *ishell.Context) {
			c.Printf("%-20s|%-20s| %-9s\n", "Name", "Groups", "Status")
			for _, unit := range config.Units {
				c.Printf("%-20s|%-20s| %-9s\n",
					unit.Name,
					strings.Join(unit.Groups, ", "),
					unit.ProcessStatus(),
				)
			}
		},
	})

	shell.AddCmd(&ishell.Cmd{
		Name: "reload",
		Help: "Reload the glorious config",
		Func: func(c *ishell.Context) {
			config, err = loadConfig(*configFileLocation)
			if err != nil {
				fmt.Println("failed to reload config: ", err)
				return
			}
			c.Printf("Reloaded glorious config from %s\n", *configFileLocation)
		},
	})

	shell.AddCmd(&ishell.Cmd{
		Name: "start",
		Help: "Start a given unit",
		Func: func(c *ishell.Context) {
			unitsToStart, err := config.GetUnits(c.Args)
			if err != nil {
				fmt.Println(err)
				return
			}

			for _, unit := range unitsToStart {
				c.Printf("starting %q...\n", unit.Name)

				if err := unit.Start(c); err != nil {
					fmt.Println(err)
				}

				c.Println("done")
			}
		},
	})

	storeCmd := &ishell.Cmd{
		Name: "store",
		Help: "Access the glorious store",
		Func: func(c *ishell.Context) {
			c.Println(c.Cmd.HelpText())
		},
	}
	storeCmd.AddCmd(&ishell.Cmd{
		Name: "get",
		Help: "Return value for given key",
		Func: func(c *ishell.Context) {
			if len(c.Args) == 0 {
				c.Println("must provide key to retrieve value for")
				return
			}

			for _, key := range c.Args {
				val, err := getInternalStoreVal(key)
				if err != nil {
					val = "(not found)"
				}
				c.Printf("%q: %q\n", key, val)
			}
		},
	})
	storeCmd.AddCmd(&ishell.Cmd{
		Name: "set",
		Help: "Stores the key/value pair in the internal store",
		Func: func(c *ishell.Context) {
			if len(c.Args) != 2 {
				c.Println("May only provide a single key value pair")
				return
			}

			if err := putInternalStoreVal(c.Args[0], c.Args[1]); err != nil {
				c.Println(err)
				return
			}

			if err := config.assertKeyChange(c.Args[0], c); err != nil {
				c.Println(err)
			}
		},
	})
	shell.AddCmd(storeCmd)

	shell.AddCmd(&ishell.Cmd{
		Name: "stop",
		Help: "Stops given units",
		Func: func(c *ishell.Context) {
			unitsToStart, err := config.GetUnits(c.Args)
			if err != nil {
				fmt.Println(err)
				return
			}

			for _, unit := range unitsToStart {
				c.Printf("stopping %q...", unit.Name)

				if err := unit.Stop(c); err != nil {
					fmt.Println(err)
				}

				c.Println("done")
			}
		},
	})

	shell.AddCmd(&ishell.Cmd{
		Name: "tail",
		Help: "Tails a unit",
		Func: func(c *ishell.Context) {
			// be cheaper if we can
			unitName := c.Args[0]
			unit, ok := config.GetUnit(unitName)
			if !ok {
				c.Printf("unknown unit %q, aborting\n", unitName)
				return
			}

			// if there are no more args, run a following tail, if
			// there's a number, run a number sliced tail
			if err := unit.Tail(c); err != nil {
				c.Println(err)
			}
		},
	})

	// run shell
	shell.Run()
}

const banner = `
       _            _
  __ _| | ___  _ __(_) ___  _   _ ___
 / _  | |/ _ \| '__| |/ _ \| | | / __|
| (_| | | (_) | |  | | (_) | |_| \__ \
 \__, |_|\___/|_|  |_|\___/ \__,_|___/
 |___/
`

func loadConfig(configFileLocation string) (*GloriousConfig, error) {
	data, err := ioutil.ReadFile(configFileLocation)
	if err != nil {
		return nil, err
	}

	var m GloriousConfig
	if err := hcl.Unmarshal(data, &m); err != nil {
		return nil, err
	}

	// We need to identify if any previous processes are running.
	//
	// For now, we'll solely support docker. Next will be PIDfile based
	// support.
	m.Groups = make(map[string][]string)
	for _, unit := range m.Units {
		if len(unit.Groups) > 0 {
			for _, group := range unit.Groups {
				if m.Groups[group] == nil {
					m.Groups[group] = make([]string, 0)
				}
				m.Groups[group] = append(m.Groups[group], unit.Name)
			}
		}

		slot, err := unit.identifySlot()
		if err != nil {
			// TODO(ttacon): wrap this error
			return nil, err
		}

		if slot == nil || slot.Provider == nil {
			return nil, fmt.Errorf("invalid provider for unit %q", unit.Name)
		}

		if strings.HasPrefix(slot.Provider.Type, "docker") {
			if err := unit.populateDockerStatus(slot); err != nil {
				return nil, err
			}
		}
	}

	return &m, nil
}

type GloriousConfig struct {
	Units  []*Unit `hcl:"unit"`
	Groups map[string][]string
}

func (g *GloriousConfig) GetUnit(name string) (*Unit, bool) {
	for _, unit := range g.Units {
		if unit.Name == name {
			return unit, true
		}

	}
	return nil, false
}

func (g *GloriousConfig) GetGroup(name string) ([]*Unit, bool) {
	if g.Groups[name] == nil {
		return nil, false
	}

	var units []*Unit
	for _, name := range g.Groups[name] {
		unit, ok := g.GetUnit(name)
		if !ok {
			return nil, false
		}

		units = append(units, unit)
	}

	return units, true
}

func (g *GloriousConfig) GetUnits(args []string) ([]*Unit, error) {
	var unitsToStart []*Unit
	for _, name := range args {
		group, ok := g.GetGroup(name)
		if ok {
			return group, nil
		}

		unit, ok := g.GetUnit(name)
		if !ok {
			return nil, errors.New(fmt.Sprintf("unknown unit %q, aborting", name))
		}

		unitsToStart = append(unitsToStart, unit)
	}

	return unitsToStart, nil
}

func (g *GloriousConfig) assertKeyChange(key string, ctxt *ishell.Context) error {
	for _, unit := range g.Units {
		for _, slot := range unit.Slots {
			if slot.Resolver["type"] != "keyword/value" {
				continue
			} else if slot.Resolver["keyword"] == key {
				if err := unit.Restart(ctxt); err != nil {
					return err
				}
			}
		}
	}

	return nil
}
