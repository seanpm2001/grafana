package navtreeimpl

import (
	"fmt"
	"sort"

	"github.com/grafana/grafana/pkg/api/dtos"
	"github.com/grafana/grafana/pkg/infra/kvstore"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/models/roletype"
	ac "github.com/grafana/grafana/pkg/services/accesscontrol"
	"github.com/grafana/grafana/pkg/services/apikey"
	"github.com/grafana/grafana/pkg/services/auth/identity"
	contextmodel "github.com/grafana/grafana/pkg/services/contexthandler/model"
	"github.com/grafana/grafana/pkg/services/dashboards"
	"github.com/grafana/grafana/pkg/services/datasources"
	"github.com/grafana/grafana/pkg/services/featuremgmt"
	"github.com/grafana/grafana/pkg/services/licensing"
	"github.com/grafana/grafana/pkg/services/navtree"
	"github.com/grafana/grafana/pkg/services/org"
	"github.com/grafana/grafana/pkg/services/pluginsintegration/pluginsettings"
	"github.com/grafana/grafana/pkg/services/pluginsintegration/pluginstore"
	pref "github.com/grafana/grafana/pkg/services/preference"
	"github.com/grafana/grafana/pkg/services/star"
	"github.com/grafana/grafana/pkg/services/supportbundles/supportbundlesimpl"
	"github.com/grafana/grafana/pkg/setting"
)

type ServiceImpl struct {
	cfg                  *setting.Cfg
	log                  log.Logger
	accessControl        ac.AccessControl
	pluginStore          pluginstore.Store
	pluginSettings       pluginsettings.Service
	starService          star.Service
	features             featuremgmt.FeatureToggles
	dashboardService     dashboards.DashboardService
	accesscontrolService ac.Service
	kvStore              kvstore.KVStore
	apiKeyService        apikey.Service
	license              licensing.Licensing

	// Navigation
	navigationAppConfig     map[string]NavigationAppConfig
	navigationAppPathConfig map[string]NavigationAppConfig
}

type NavigationAppConfig struct {
	SectionID  string
	SortWeight int64
	Text       string
	Icon       string
}

func ProvideService(cfg *setting.Cfg, accessControl ac.AccessControl, pluginStore pluginstore.Store, pluginSettings pluginsettings.Service, starService star.Service, features featuremgmt.FeatureToggles, dashboardService dashboards.DashboardService, accesscontrolService ac.Service, kvStore kvstore.KVStore, apiKeyService apikey.Service, license licensing.Licensing) navtree.Service {
	service := &ServiceImpl{
		cfg:                  cfg,
		log:                  log.New("navtree service"),
		accessControl:        accessControl,
		pluginStore:          pluginStore,
		pluginSettings:       pluginSettings,
		starService:          starService,
		features:             features,
		dashboardService:     dashboardService,
		accesscontrolService: accesscontrolService,
		kvStore:              kvStore,
		apiKeyService:        apiKeyService,
		license:              license,
	}

	service.readNavigationSettings()

	return service
}

//nolint:gocyclo
func (s *ServiceImpl) GetNavTree(c *contextmodel.ReqContext, prefs *pref.Preference) (*navtree.NavTreeRoot, error) {
	hasAccess := ac.HasAccess(s.accessControl, c)
	treeRoot := &navtree.NavTreeRoot{}

	treeRoot.AddSection(s.getHomeNode(c, prefs))

	if hasAccess(ac.EvalPermission(dashboards.ActionDashboardsRead)) {
		starredItemsLinks, err := s.buildStarredItemsNavLinks(c)
		if err != nil {
			return nil, err
		}

		treeRoot.AddSection(&navtree.NavLink{
			Text:           "Starred",
			Id:             "starred",
			Icon:           "star",
			SortWeight:     navtree.WeightSavedItems,
			Children:       starredItemsLinks,
			EmptyMessageId: "starred-empty",
			Url:            s.cfg.AppSubURL + "/dashboards?starred",
		})
	}

	if c.IsPublicDashboardView() || hasAccess(ac.EvalAny(
		ac.EvalPermission(dashboards.ActionFoldersRead), ac.EvalPermission(dashboards.ActionFoldersCreate),
		ac.EvalPermission(dashboards.ActionDashboardsRead), ac.EvalPermission(dashboards.ActionDashboardsCreate)),
	) {
		dashboardChildLinks := s.buildDashboardNavLinks(c)

		dashboardLink := &navtree.NavLink{
			Text:       "Dashboards",
			Id:         navtree.NavIDDashboards,
			SubTitle:   "Create and manage dashboards to visualize your data",
			Icon:       "apps",
			Url:        s.cfg.AppSubURL + "/dashboards",
			SortWeight: navtree.WeightDashboard,
			Children:   dashboardChildLinks,
		}

		treeRoot.AddSection(dashboardLink)
	}

	if s.cfg.ExploreEnabled && hasAccess(ac.EvalPermission(ac.ActionDatasourcesExplore)) {
		exploreChildNavLinks := s.buildExploreNavLinks(c)
		treeRoot.AddSection(&navtree.NavLink{
			Text:       "Explore",
			Id:         navtree.NavIDExplore,
			SubTitle:   "Explore your data",
			Icon:       "compass",
			SortWeight: navtree.WeightExplore,
			Url:        s.cfg.AppSubURL + "/explore",
			Children:   exploreChildNavLinks,
		})
	}

	if s.cfg.ProfileEnabled && c.IsSignedIn {
		treeRoot.AddSection(s.getProfileNode(c))
	}

	_, uaIsDisabledForOrg := s.cfg.UnifiedAlerting.DisabledOrgs[c.SignedInUser.GetOrgID()]
	uaVisibleForOrg := s.cfg.UnifiedAlerting.IsEnabled() && !uaIsDisabledForOrg

	if s.cfg.AlertingEnabled != nil && *(s.cfg.AlertingEnabled) {
		if legacyAlertSection := s.buildLegacyAlertNavLinks(c); legacyAlertSection != nil {
			treeRoot.AddSection(legacyAlertSection)
		}
		if s.features.IsEnabled(c.Req.Context(), featuremgmt.FlagAlertingPreviewUpgrade) && !uaIsDisabledForOrg {
			if alertingSection := s.buildAlertNavLinks(c); alertingSection != nil {
				treeRoot.AddSection(alertingSection)
			}
		}
	} else if uaVisibleForOrg {
		if alertingSection := s.buildAlertNavLinks(c); alertingSection != nil {
			treeRoot.AddSection(alertingSection)
		}
	}

	if connectionsSection := s.buildDataConnectionsNavLink(c); connectionsSection != nil {
		treeRoot.AddSection(connectionsSection)
	}

	orgAdminNode, err := s.getAdminNode(c)

	if orgAdminNode != nil {
		treeRoot.AddSection(orgAdminNode)
	} else if err != nil {
		return nil, err
	}

	s.addHelpLinks(treeRoot, c)

	if err := s.addAppLinks(treeRoot, c); err != nil {
		return nil, err
	}

	return treeRoot, nil
}

func (s *ServiceImpl) getHomeNode(c *contextmodel.ReqContext, prefs *pref.Preference) *navtree.NavLink {
	homeUrl := s.cfg.AppSubURL + "/"
	if !c.IsSignedIn && !s.cfg.AnonymousEnabled {
		homeUrl = s.cfg.AppSubURL + "/login"
	} else {
		homePage := s.cfg.HomePage

		if prefs.HomeDashboardID == 0 && len(homePage) > 0 {
			homeUrl = homePage
		}
	}

	homeNode := &navtree.NavLink{
		Text:       "Home",
		Id:         "home",
		Url:        homeUrl,
		Icon:       "home-alt",
		SortWeight: navtree.WeightHome,
	}
	return homeNode
}

func isSupportBundlesEnabled(s *ServiceImpl) bool {
	return s.cfg.SectionWithEnvOverrides("support_bundles").Key("enabled").MustBool(true)
}

// don't need to show the full commit hash in the UI
// let's substring to 10 chars like local git does automatically
func getShortCommitHash(commitHash string, maxLength int) string {
	if len(commitHash) > maxLength {
		return commitHash[:maxLength]
	}
	return commitHash
}

func (s *ServiceImpl) addHelpLinks(treeRoot *navtree.NavTreeRoot, c *contextmodel.ReqContext) {
	if s.cfg.HelpEnabled {
		helpVersion := fmt.Sprintf(`%s v%s (%s)`, setting.ApplicationName, setting.BuildVersion, getShortCommitHash(setting.BuildCommit, 10))
		if s.cfg.AnonymousHideVersion && !c.IsSignedIn {
			helpVersion = setting.ApplicationName
		}

		helpNode := &navtree.NavLink{
			Text:       "Help",
			SubTitle:   helpVersion,
			Id:         "help",
			Url:        "#",
			Icon:       "question-circle",
			SortWeight: navtree.WeightHelp,
			Children:   []*navtree.NavLink{},
		}

		treeRoot.AddSection(helpNode)

		hasAccess := ac.HasAccess(s.accessControl, c)
		supportBundleAccess := ac.EvalAny(
			ac.EvalPermission(supportbundlesimpl.ActionRead),
			ac.EvalPermission(supportbundlesimpl.ActionCreate),
		)

		if isSupportBundlesEnabled(s) && hasAccess(supportBundleAccess) {
			supportBundleNode := &navtree.NavLink{
				Text:       "Support bundles",
				Id:         "support-bundles",
				Url:        "/support-bundles",
				Icon:       "wrench",
				SortWeight: navtree.WeightHelp,
			}

			helpNode.Children = append(helpNode.Children, supportBundleNode)
		}
	}
}

func (s *ServiceImpl) getProfileNode(c *contextmodel.ReqContext) *navtree.NavLink {
	// Only set login if it's different from the name
	var login string
	if c.SignedInUser.GetLogin() != c.SignedInUser.GetDisplayName() {
		login = c.SignedInUser.GetLogin()
	}
	gravatarURL := dtos.GetGravatarUrl(s.cfg, c.SignedInUser.GetEmail())

	children := []*navtree.NavLink{
		{
			Text: "Profile", Id: "profile/settings", Url: s.cfg.AppSubURL + "/profile", Icon: "sliders-v-alt",
		},
	}

	children = append(children, &navtree.NavLink{
		Text: "Notification history", Id: "profile/notifications", Url: s.cfg.AppSubURL + "/profile/notifications", Icon: "bell",
	})

	if s.cfg.AddChangePasswordLink() {
		children = append(children, &navtree.NavLink{
			Text: "Change password", Id: "profile/password", Url: s.cfg.AppSubURL + "/profile/password",
			Icon: "lock",
		})
	}

	if !s.cfg.DisableSignoutMenu {
		// add sign out first
		children = append(children, &navtree.NavLink{
			Text:         "Sign out",
			Id:           "sign-out",
			Url:          s.cfg.AppSubURL + "/logout",
			Icon:         "arrow-from-right",
			Target:       "_self",
			HideFromTabs: true,
		})
	}

	return &navtree.NavLink{
		Text:       c.SignedInUser.GetDisplayName(),
		SubTitle:   login,
		Id:         "profile",
		Img:        gravatarURL,
		Url:        s.cfg.AppSubURL + "/profile",
		SortWeight: navtree.WeightProfile,
		Children:   children,
		RoundIcon:  true,
	}
}

func (s *ServiceImpl) buildStarredItemsNavLinks(c *contextmodel.ReqContext) ([]*navtree.NavLink, error) {
	starredItemsChildNavs := []*navtree.NavLink{}

	userID, _ := identity.UserIdentifier(c.SignedInUser.GetNamespacedID())
	query := star.GetUserStarsQuery{
		UserID: userID,
	}

	starredDashboardResult, err := s.starService.GetByUser(c.Req.Context(), &query)
	if err != nil {
		return nil, err
	}

	if len(starredDashboardResult.UserStars) > 0 {
		var ids []int64
		for id := range starredDashboardResult.UserStars {
			ids = append(ids, id)
		}
		starredDashboards, err := s.dashboardService.GetDashboards(c.Req.Context(), &dashboards.GetDashboardsQuery{DashboardIDs: ids, OrgID: c.SignedInUser.GetOrgID()})
		if err != nil {
			return nil, err
		}
		// Set a loose limit to the first 50 starred dashboards found
		if len(starredDashboards) > 50 {
			starredDashboards = starredDashboards[:50]
		}

		sort.Slice(starredDashboards, func(i, j int) bool {
			return starredDashboards[i].Title < starredDashboards[j].Title
		})
		for _, starredItem := range starredDashboards {
			starredItemsChildNavs = append(starredItemsChildNavs, &navtree.NavLink{
				Id:   "starred/" + starredItem.UID,
				Text: starredItem.Title,
				Url:  starredItem.GetURL(),
			})
		}
	}

	return starredItemsChildNavs, nil
}

func (s *ServiceImpl) buildDashboardNavLinks(c *contextmodel.ReqContext) []*navtree.NavLink {
	hasAccess := ac.HasAccess(s.accessControl, c)

	dashboardChildNavs := []*navtree.NavLink{}

	dashboardChildNavs = append(dashboardChildNavs, &navtree.NavLink{
		Text: "Playlists", SubTitle: "Groups of dashboards that are displayed in a sequence", Id: "dashboards/playlists", Url: s.cfg.AppSubURL + "/playlists", Icon: "presentation-play",
	})

	if c.IsSignedIn {
		if s.cfg.SnapshotEnabled {
			dashboardChildNavs = append(dashboardChildNavs, &navtree.NavLink{
				Text:     "Snapshots",
				SubTitle: "Interactive, publically available, point-in-time representations of dashboards",
				Id:       "dashboards/snapshots",
				Url:      s.cfg.AppSubURL + "/dashboard/snapshots",
				Icon:     "camera",
			})
		}

		dashboardChildNavs = append(dashboardChildNavs, &navtree.NavLink{
			Text:     "Library panels",
			SubTitle: "Reusable panels that can be added to multiple dashboards",
			Id:       "dashboards/library-panels",
			Url:      s.cfg.AppSubURL + "/library-panels",
			Icon:     "library-panel",
		})

		if s.features.IsEnabled(c.Req.Context(), featuremgmt.FlagPublicDashboards) && s.cfg.PublicDashboardsEnabled {
			dashboardChildNavs = append(dashboardChildNavs, &navtree.NavLink{
				Text: "Public dashboards",
				Id:   "dashboards/public",
				Url:  s.cfg.AppSubURL + "/dashboard/public",
				Icon: "library-panel",
			})
		}
	}

	if hasAccess(ac.EvalPermission(dashboards.ActionDashboardsCreate)) {
		dashboardChildNavs = append(dashboardChildNavs, &navtree.NavLink{
			Text: "New dashboard", Icon: "plus", Url: s.cfg.AppSubURL + "/dashboard/new", HideFromTabs: true, Id: "dashboards/new", IsCreateAction: true,
		})

		dashboardChildNavs = append(dashboardChildNavs, &navtree.NavLink{
			Text: "Import dashboard", SubTitle: "Import dashboard from file or Grafana.com", Id: "dashboards/import", Icon: "plus",
			Url: s.cfg.AppSubURL + "/dashboard/import", HideFromTabs: true, IsCreateAction: true,
		})
	}

	return dashboardChildNavs
}

func (s *ServiceImpl) buildLegacyAlertNavLinks(c *contextmodel.ReqContext) *navtree.NavLink {
	var alertChildNavs []*navtree.NavLink
	alertChildNavs = append(alertChildNavs, &navtree.NavLink{
		Text: "Alert rules", Id: "alert-list-legacy", Url: s.cfg.AppSubURL + "/alerting-legacy/list", Icon: "list-ul",
	})

	if c.SignedInUser.HasRole(roletype.RoleEditor) {
		alertChildNavs = append(alertChildNavs, &navtree.NavLink{
			Text: "Notification channels", Id: "channels", Url: s.cfg.AppSubURL + "/alerting-legacy/notifications",
			Icon: "comment-alt-share",
		})
	}

	if s.features.IsEnabled(c.Req.Context(), featuremgmt.FlagAlertingPreviewUpgrade) && c.HasRole(org.RoleAdmin) {
		alertChildNavs = append(alertChildNavs, &navtree.NavLink{
			Text: "Upgrade Alerting", Id: "alerting-upgrade", Url: s.cfg.AppSubURL + "/alerting-legacy/upgrade",
			SubTitle: "Upgrade your existing legacy alerts and notification channels to the new Grafana Alerting",
			Icon:     "cog",
		})
	}

	var alertNav = navtree.NavLink{
		Text:       "Alerting",
		SubTitle:   "Learn about problems in your systems moments after they occur",
		Id:         "alerting-legacy",
		Icon:       "bell",
		Children:   alertChildNavs,
		SortWeight: navtree.WeightAlerting,
		Url:        s.cfg.AppSubURL + "/alerting-legacy",
	}

	return &alertNav
}

func (s *ServiceImpl) buildAlertNavLinks(c *contextmodel.ReqContext) *navtree.NavLink {
	hasAccess := ac.HasAccess(s.accessControl, c)
	var alertChildNavs []*navtree.NavLink

	if hasAccess(ac.EvalAny(ac.EvalPermission(ac.ActionAlertingRuleRead), ac.EvalPermission(ac.ActionAlertingRuleExternalRead))) {
		alertChildNavs = append(alertChildNavs, &navtree.NavLink{
			Text: "Alert rules", SubTitle: "Rules that determine whether an alert will fire", Id: "alert-list", Url: s.cfg.AppSubURL + "/alerting/list", Icon: "list-ul",
		})
	}

	if hasAccess(ac.EvalAny(ac.EvalPermission(ac.ActionAlertingNotificationsRead), ac.EvalPermission(ac.ActionAlertingNotificationsExternalRead))) {
		alertChildNavs = append(alertChildNavs, &navtree.NavLink{
			Text: "Contact points", SubTitle: "Choose how to notify your  contact points when an alert instance fires", Id: "receivers", Url: s.cfg.AppSubURL + "/alerting/notifications",
			Icon: "comment-alt-share",
		})
		alertChildNavs = append(alertChildNavs, &navtree.NavLink{Text: "Notification policies", SubTitle: "Determine how alerts are routed to contact points", Id: "am-routes", Url: s.cfg.AppSubURL + "/alerting/routes", Icon: "sitemap"})
	}

	if hasAccess(ac.EvalAny(ac.EvalPermission(ac.ActionAlertingInstanceRead), ac.EvalPermission(ac.ActionAlertingInstancesExternalRead))) {
		alertChildNavs = append(alertChildNavs, &navtree.NavLink{Text: "Silences", SubTitle: "Stop notifications from one or more alerting rules", Id: "silences", Url: s.cfg.AppSubURL + "/alerting/silences", Icon: "bell-slash"})
		alertChildNavs = append(alertChildNavs, &navtree.NavLink{Text: "Active notifications", SubTitle: "See active notifications", Id: "groups", Url: s.cfg.AppSubURL + "/alerting/groups", Icon: "layer-group"})
	}

	if c.SignedInUser.GetOrgRole() == org.RoleAdmin {
		alertChildNavs = append(alertChildNavs, &navtree.NavLink{
			Text: "Admin", Id: "alerting-admin", Url: s.cfg.AppSubURL + "/alerting/admin",
			Icon: "cog",
		})
	}

	if hasAccess(ac.EvalAny(ac.EvalPermission(ac.ActionAlertingRuleCreate), ac.EvalPermission(ac.ActionAlertingRuleExternalWrite))) {
		alertChildNavs = append(alertChildNavs, &navtree.NavLink{
			Text: "Create alert rule", SubTitle: "Create an alert rule", Id: "alert",
			Icon: "plus", Url: s.cfg.AppSubURL + "/alerting/new", HideFromTabs: true, IsCreateAction: true,
		})
	}

	if len(alertChildNavs) > 0 {
		var alertNav = navtree.NavLink{
			Text:       "Alerting",
			SubTitle:   "Learn about problems in your systems moments after they occur",
			Id:         navtree.NavIDAlerting,
			Icon:       "bell",
			Children:   alertChildNavs,
			SortWeight: navtree.WeightAlerting,
			Url:        s.cfg.AppSubURL + "/alerting",
		}

		return &alertNav
	}

	return nil
}

func (s *ServiceImpl) buildDataConnectionsNavLink(c *contextmodel.ReqContext) *navtree.NavLink {
	hasAccess := ac.HasAccess(s.accessControl, c)

	var children []*navtree.NavLink
	var navLink *navtree.NavLink

	baseUrl := s.cfg.AppSubURL + "/connections"

	if hasAccess(datasources.ConfigurationPageAccess) {
		// Add new connection
		children = append(children, &navtree.NavLink{
			Id:       "connections-add-new-connection",
			Text:     "Add new connection",
			SubTitle: "Browse and create new connections",
			Url:      baseUrl + "/add-new-connection",
			Children: []*navtree.NavLink{},
			Keywords: []string{"csv", "graphite", "json", "loki", "prometheus", "sql", "tempo"},
		})

		// Data sources
		children = append(children, &navtree.NavLink{
			Id:       "connections-datasources",
			Text:     "Data sources",
			SubTitle: "View and manage your connected data source connections",
			Url:      baseUrl + "/datasources",
			Children: []*navtree.NavLink{},
		})
	}

	if len(children) > 0 {
		// Connections (main)
		navLink = &navtree.NavLink{
			Text:       "Connections",
			Icon:       "adjust-circle",
			Id:         "connections",
			Url:        baseUrl,
			Children:   children,
			SortWeight: navtree.WeightDataConnections,
		}

		return navLink
	}
	return nil
}

func (s *ServiceImpl) buildExploreNavLinks(c *contextmodel.ReqContext) []*navtree.NavLink {
	exploreChildNavs := []*navtree.NavLink{}
	if s.features.IsEnabled(c.Req.Context(), featuremgmt.FlagDatatrails) {
		exploreChildNavs = append(exploreChildNavs, &navtree.NavLink{
			Text:     "Metrics",
			SubTitle: "Queryless exploration of your metrics",
			Id:       "explore/metrics",
			Url:      s.cfg.AppSubURL + "/explore/metrics",
			Icon:     "code-branch",
		})
	}
	return exploreChildNavs
}
