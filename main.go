package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"net/url"
	"os"
	"strings"

	"code.cloudfoundry.org/cli/v8/plugin"
	"github.com/cloudfoundry/go-cfclient/v3/client"
	"github.com/cloudfoundry/go-cfclient/v3/config"
	"github.com/cloudfoundry/go-cfclient/v3/resource"
)

type lookupRoute struct{}

const usageDescription = "cf lookup-route ROUTE_URL"

func main() {
	plugin.Start(new(lookupRoute))
}

func (l lookupRoute) Run(cliConnection plugin.CliConnection, args []string) {
	var err error
	defer func() {
		if err != nil {
			fmt.Printf("error: %s\n", err.Error())
			os.Exit(1)
		}
	}()

	if args[0] == "CLI-MESSAGE-UNINSTALL" {
		return
	}
	flags := flag.NewFlagSet("lookup-route", flag.ContinueOnError)
	err = flags.Parse(args[1:])
	if err != nil {
		return
	}

	if len(flags.Args()) == 0 {
		err = fmt.Errorf("missing ROUTE_URL argument. Usage: %s", usageDescription)
		return
	}

	hostName := flags.Args()[0]

	hasApiEndpoint, err := cliConnection.HasAPIEndpoint()
	if err != nil || !hasApiEndpoint {
		err = fmt.Errorf("no API endpoint targeted. Run 'cf login' or 'cf api <API_ENDPOINT>' to set one")
		return
	}

	loggedIn, err := cliConnection.IsLoggedIn()
	if err != nil {
		return
	}
	if !loggedIn {
		err = fmt.Errorf("not authenticated. Run 'cf login' first")
		return
	}

	cfc, err := createCfClient()
	if err != nil {
		return
	}

	route, err := findRoute(cfc, hostName)
	if err != nil {
		return
	}
	if route.Destinations == nil || len(route.Destinations) == 0 {
		err = fmt.Errorf("route '%s' is not bound to any applications", hostName)
		return
	}

	err = lookup(cfc, route)
	if err != nil {
		return
	}
}

func (l lookupRoute) GetMetadata() plugin.PluginMetadata {
	return plugin.PluginMetadata{
		Name: "lookup-route",
		// See https://github.com/cloudfoundry/cli-plugin-repo/README.md for version publishing procedure.
		Version: plugin.VersionType{
			Major: 0,
			Minor: 2,
			Build: 1,
		},
		Commands: []plugin.Command{
			{
				Name:     "lookup-route",
				HelpText: "Cloud Foundry CLI plugin to identify applications, a given route is pointing to.",
				UsageDetails: plugin.Usage{
					Usage: usageDescription,
				},
			},
		},
	}
}

func createCfClient() (*client.Client, error) {
	cfg, err := config.NewFromCFHome()
	if err != nil {
		return &client.Client{}, err
	}

	cfc, err := client.New(cfg)
	if err != nil {
		return &client.Client{}, err
	}

	return cfc, nil
}

func retrieveDomains(cfc *client.Client, domainName string) ([]*resource.Domain, error) {
	domainOpts := client.NewDomainListOptions()
	domainOpts.Names.Values = append(domainOpts.Names.Values, domainName)
	domains, err := cfc.Domains.ListAll(context.Background(), domainOpts)
	if err != nil {
		return nil, err
	}
	return domains, nil
}

func parseDomain(cfc *client.Client, query string) (*resource.Domain, string, *url.URL, error) {
	normalizedQuery := strings.TrimSpace(query)
	routeUrl, err := url.Parse(normalizedQuery)
	if err != nil {
		return &resource.Domain{}, "", &url.URL{}, fmt.Errorf("failed to parse route %q: %w", query, err)
	}
	// If no scheme is provided, default to https so the input is parsed as a URL host, not a path.
	// The scheme itself is not used for route lookup.
	if routeUrl.Scheme == "" {
		routeUrl, err = url.Parse("https://" + normalizedQuery)
		if err != nil {
			return &resource.Domain{}, "", &url.URL{}, fmt.Errorf("failed to parse route %q with 'https://' prefix: %w", query, err)
		}
	}

	domains, err := retrieveDomains(cfc, routeUrl.Hostname())
	if err != nil {
		return &resource.Domain{}, routeUrl.Hostname(), routeUrl, err
	}

	if len(domains) > 0 {
		return domains[0], routeUrl.Hostname(), routeUrl, nil
	}

	hostName, domainName, found := strings.Cut(routeUrl.Hostname(), ".")
	if !found {
		return &resource.Domain{}, "", routeUrl, fmt.Errorf("invalid route '%s': expected a domain (e.g., 'my.example.com')", routeUrl.Hostname())
	}

	domains, err = retrieveDomains(cfc, domainName)
	if err != nil {
		return &resource.Domain{}, hostName, routeUrl, fmt.Errorf("failed to look up domain '%s': %w", domainName, err)
	}
	if len(domains) == 0 {
		return &resource.Domain{}, hostName, routeUrl, fmt.Errorf("domain '%s' not found", domainName)
	}

	return domains[0], hostName, routeUrl, nil
}

func findRoute(cfc *client.Client, query string) (*resource.Route, error) {
	domain, hostName, routeUrl, err := parseDomain(cfc, query)
	if err != nil {
		return &resource.Route{}, err
	}

	opts := client.NewRouteListOptions()
	opts.Hosts.Values = append(opts.Hosts.Values, hostName)
	opts.DomainGUIDs.Values = append(opts.DomainGUIDs.Values, domain.GUID)
	opts.Paths.Values = append(opts.Paths.Values, routeUrl.Path)

	routes, err := cfc.Routes.ListAll(context.Background(), opts)
	if err != nil {
		return &resource.Route{}, fmt.Errorf("failed to retrieve route '%s': %w", query, err)
	}

	if len(routes) > 0 {
		return routes[0], nil
	}
	// Retry with wildcard host fallback.
	opts.Hosts.Values = append(opts.Hosts.Values, "*")
	routes, err = cfc.Routes.ListAll(context.Background(), opts)
	if err != nil {
		return &resource.Route{}, fmt.Errorf("failed to retrieve wildcard route for '%s': %w", query, err)
	}
	if len(routes) == 0 {
		return &resource.Route{}, fmt.Errorf("route '%s' not found", routeUrl.Hostname())
	}

	return routes[0], nil
}

func getBatchEndIdx(routeDestCount int, batchCount int, currentIdx int) int {
	batchEndIdx := currentIdx*batchCount + batchCount
	if batchEndIdx > routeDestCount {
		batchEndIdx = routeDestCount
	}
	return batchEndIdx
}

func resolveApps(cfc *client.Client, route *resource.Route) ([]*resource.App, error) {
	var appGuids []string
	var apps []*resource.App

	for _, destination := range route.Destinations {
		appGuids = append(appGuids, *destination.App.GUID)
	}

	// Query apps in batches to reduce API calls.
	routeDestCount := len(appGuids)
	batchSize := 100
	batchCount := int(math.Ceil(float64(routeDestCount) / float64(batchSize)))
	opts := client.NewAppListOptions()
	opts.PerPage = batchSize

	for i := 0; i < batchCount; i++ {
		opts.GUIDs.Values = appGuids[i*batchSize : getBatchEndIdx(routeDestCount, batchSize, i)]
		batchApps, err := cfc.Applications.ListAll(context.Background(), opts)
		if err != nil {
			return []*resource.App{}, err
		}
		apps = append(apps, batchApps...)
	}
	return apps, nil
}

func lookup(cfc *client.Client, route *resource.Route) error {
	apps, err := resolveApps(cfc, route)
	if err != nil {
		return err
	}
	if len(apps) == 0 {
		return fmt.Errorf("no applications found for this route")
	}

	spaceRel := apps[0].Relationships.Space.Data
	if spaceRel == nil || spaceRel.GUID == "" {
		return fmt.Errorf("failed to resolve organization/space for app '%s': no space relationship data", apps[0].Name)
	}

	// All the apps sharing a route must be in the same org and space.
	space, org, err := cfc.Spaces.GetIncludeOrganization(context.Background(), spaceRel.GUID)
	if err != nil {
		return fmt.Errorf("failed to resolve organization/space for app '%s': %w", apps[0].Name, err)
	}

	fmt.Printf("Bound to:\nOrganization: %s (%s)\n", org.Name, org.GUID)
	fmt.Printf("Space       : %s (%s)\n", space.Name, space.GUID)
	for _, app := range apps {
		fmt.Printf("App         : %s (%s)\n", app.Name, app.GUID)
	}

	fmt.Printf("\nTo target this org/space, run:\n  cf target -o %s -s %s\n\n", org.Name, space.Name)
	return nil
}
