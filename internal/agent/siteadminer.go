package agent

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/files"
)

// Adminer lives in the site's folder, outside public/ (so it cannot be
// opened without the settings of its page), behind a page in public/ with
// a name nobody guesses.
const (
	adminerDir    = "/.basalt-adminer"
	adminerCore   = adminerDir + "/adminer.php"
	adminerMarker = "// Basalt Adminer "
)

var adminerPageRE = regexp.MustCompile(`^adminer-[a-z2-7]{26}\.php$`)

// adminerPage connects Adminer to this server's MariaDB only: the server
// is not asked for, so the page cannot reach other hosts. Parameters stay
// untyped (the compiled Adminer has none; a typed one would not match).
func adminerPage(version string) string {
	return "<?php\n" + adminerMarker + version + ". Written by Basalt; removed with Adminer from the panel.\n" +
		`function adminer_object() {
	class BasaltAdminer extends \Adminer\Adminer {
		function credentials(): array {
			return array('localhost', $_GET['username'], \Adminer\get_password());
		}
		function loginFormField($name, $heading, $value): string {
			return $name == 'server' || $name == 'driver' ? '' : parent::loginFormField($name, $heading, $value);
		}
	}
	return new BasaltAdminer;
}
include __DIR__ . '/../.basalt-adminer/adminer.php';
`
}

func (a *Agent) siteAdminer(ctx context.Context, req agentproto.SiteAdminer) (agentproto.SiteAdminerResponse, error) {
	site, err := a.sites.open(req.SiteID)
	if err != nil {
		return agentproto.SiteAdminerResponse{}, err
	}
	defer site.Close()
	pages, version, err := adminerPages(site)
	if err != nil {
		return agentproto.SiteAdminerResponse{}, err
	}
	switch req.Op {
	case agentproto.AdminerStatus:
		if len(pages) == 0 {
			return agentproto.SiteAdminerResponse{}, nil
		}
		return agentproto.SiteAdminerResponse{Path: "/" + pages[0], Version: version}, nil
	case agentproto.AdminerRemove:
		return agentproto.SiteAdminerResponse{}, removeAdminer(site, pages)
	case agentproto.AdminerInstall:
	default:
		return agentproto.SiteAdminerResponse{}, fmt.Errorf("unknown operation %q", req.Op)
	}

	f := a.pinned(adminerPHP, a.adminer)
	path, err := a.downloadPinned(ctx, f)
	if err != nil {
		return agentproto.SiteAdminerResponse{}, fmt.Errorf("download Adminer: %w", err)
	}
	// A new install is a new address: the old page goes.
	if err := removeAdminer(site, pages); err != nil {
		return agentproto.SiteAdminerResponse{}, err
	}
	if err := site.Mkdir(adminerDir); err != nil && !errors.Is(err, files.ErrExists) {
		return agentproto.SiteAdminerResponse{}, err
	}
	if err := copyIntoSite(site, adminerCore, path); err != nil {
		return agentproto.SiteAdminerResponse{}, err
	}
	if err := site.Chmod(adminerCore, 0o600); err != nil {
		return agentproto.SiteAdminerResponse{}, err
	}
	version = adminerVersion(f.Name)
	page := "adminer-" + strings.ToLower(rand.Text()) + ".php"
	if err := site.WriteFile("/public/"+page, []byte(adminerPage(version))); err != nil {
		return agentproto.SiteAdminerResponse{}, err
	}
	if err := site.Chmod("/public/"+page, 0o600); err != nil {
		return agentproto.SiteAdminerResponse{}, err
	}
	return agentproto.SiteAdminerResponse{Path: "/" + page, Version: version}, nil
}

// adminerPages finds the pages Basalt wrote in public/ and the version
// the first one names.
func adminerPages(site *files.Site) ([]string, string, error) {
	entries, err := site.List("/public")
	if err != nil {
		return nil, "", err
	}
	var pages []string
	version := ""
	for _, e := range entries {
		if e.Type != "file" || !adminerPageRE.MatchString(e.Name) {
			continue
		}
		b, _, err := site.Read("/public/"+e.Name, 0, 256)
		if err != nil {
			continue
		}
		head, _, _ := strings.Cut(strings.TrimPrefix(string(b), "<?php\n"), "\n")
		v, ok := strings.CutPrefix(head, adminerMarker)
		if !ok {
			continue
		}
		if version == "" {
			version, _, _ = strings.Cut(v, ". ")
		}
		pages = append(pages, e.Name)
	}
	return pages, version, nil
}

func removeAdminer(site *files.Site, pages []string) error {
	for _, p := range pages {
		if err := site.Delete("/public/" + p); err != nil {
			return err
		}
	}
	if _, err := site.Stat(adminerDir); err == nil {
		return site.Delete(adminerDir)
	}
	return nil
}

// adminerVersion reads "6.1.2" from adminer-6.1.2-mysql-en.php.
func adminerVersion(name string) string {
	v := strings.TrimPrefix(name, "adminer-")
	v, _, _ = strings.Cut(v, "-")
	return strings.TrimSuffix(v, ".php")
}
