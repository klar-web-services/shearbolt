package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	neturl "net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/klar-web-services/shearbolt/internal/setup"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/zosia"
)

// zosiaEnable is what `shearbolt zosia enable` does once its file changes are written: it shows
// them, puts a secret into the CI, asks Zosia how many projects the account's plan has room
// for, and has the user say which projects send when they do not all fit.
func (s setupRun) zosiaEnable(root string, p *plan.Plan, o zosiaOpts) int {
	c := s.zosiaCI(root, p, o)
	// The note that says where to add the secret by hand goes when this run puts one there, or
	// gh shows that it is there already.
	var byHand []plan.Note
	if c.connect || c.has {
		byHand = slices.DeleteFunc(slices.Clone(p.Notes), func(n plan.Note) bool { return n.Code != "ZOSIA_SECRET" })
		p.Notes = slices.DeleteFunc(p.Notes, func(n plan.Note) bool { return n.Code == "ZOSIA_SECRET" })
	}
	s.printChanges(p)
	s.printNotes(p)
	if len(p.Changes) > 0 {
		fmt.Fprintf(s.stdout, "\n%s\n", zosiaReview)
	}
	code := s.zosiaConnect(root, p, o, c, bufio.NewReaderSize(s.d.stdin, maxAnswer+2), byHand)
	// The steps before end on their own lines, so what still blocks a gate is said after them.
	var blocked []string
	for _, n := range p.Notes {
		if n.Code == "UNSUPPORTED_EDIT" || n.Code == "ZOSIA_GATE_TOO_OLD" {
			blocked = append(blocked, fmt.Sprintf("%s:%d", n.File, n.Line))
		}
	}
	if blocked = slices.Compact(blocked); len(blocked) > 0 {
		fmt.Fprintf(s.stdout, "\nNo reports are sent yet from %s: see the notes above.\n", termSafe(strings.Join(blocked, ", "), false))
	}
	return code
}

// zosiaCI is what is known of the repository's CI and its secret when enable begins.
type zosiaCI struct {
	terminal bool
	// The repository as gh names it, whether it has ZOSIA_SECRET, and whether gh could tell and
	// can set it.
	repo       string
	has, known bool
	// The secret of the environment. With one there is a secret to ask the plan with, and none
	// is made.
	secret  string
	connect bool
}

func (s setupRun) zosiaCI(root string, p *plan.Plan, o zosiaOpts) zosiaCI {
	// With --connect it behaves as without a terminal wherever it runs: it asks nothing and shows no
	// secret. An agent's commands may well run in a terminal.
	c := zosiaCI{terminal: s.canPaste() && !o.connect, secret: strings.TrimSpace(s.d.getenv("ZOSIA_SECRET"))}
	c.repo, c.has, c.known = s.secretRepo(root, p)
	c.connect = c.secret == "" && (c.terminal || o.connect)
	return c
}

// zosiaConnect puts a secret into the CI and settles which projects send. byHand are the notes
// to show when no secret was put there.
func (s setupRun) zosiaConnect(root string, p *plan.Plan, o zosiaOpts, c zosiaCI, in *bufio.Reader, byHand []plan.Note) int {
	code, secret := 0, c.secret
	switch {
	case c.connect:
		var placed bool
		if secret, placed, code = s.zosiaSecret(root, p, c.repo, c.has, c.known, c.terminal, o, in); !placed {
			for _, n := range byHand {
				fmt.Fprintln(s.stdout, noteLine(n))
			}
		}
	case c.has:
		fmt.Fprintf(s.stdout, "\nZOSIA_SECRET is already set for %s, so there is no secret to add. Shearbolt cannot read it or check that Zosia still knows it: the scan log of the next CI run says whether the report was sent. `gh secret set ZOSIA_SECRET` replaces it.\n", termSafe(c.repo, false))
	}
	if code == 0 {
		code = s.zosiaProjects(root, o, secret, c.connect, c.terminal, in)
	}
	return code
}

// zosiaAfterSetup is that for an interactive setup that was told to send to Zosia and has
// written its files. Setup has said where the secret goes by hand already.
func (s setupRun) zosiaAfterSetup(root string, in *bufio.Reader) int {
	p := setup.Zosia(root, true, false)
	if p.Error != nil {
		return 0
	}
	o := zosiaOpts{on: true}
	c := s.zosiaCI(root, p, o)
	if !c.connect && c.secret == "" {
		return 0
	}
	return s.zosiaConnect(root, p, o, c, in, nil)
}

// zosiaTarget is where connect and the plan request go: where reports go. problem says why
// nothing may be sent there: a secret travels in these requests, so the address is https, or this
// machine.
func (s setupRun) zosiaTarget() (url, problem string) {
	url = s.d.getenv("ZOSIA_URL")
	if url == "" {
		return zosia.DefaultURL, ""
	}
	u, err := neturl.Parse(url)
	local := err == nil && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")
	if err != nil || u.Host == "" || u.Scheme != "https" && !(u.Scheme == "http" && local) {
		return url, "ZOSIA_URL is not an https address"
	}
	return url, ""
}

// zosiaSaid is a failed request in words: Zosia's code and message, or the error.
func zosiaSaid(err error) string {
	var ze *zosia.Error
	if errors.As(err, &ze) {
		return fmt.Sprintf(" (%s): %s", termSafe(ze.Code, false), termSafe(ze.Message, false))
	}
	return ": " + termSafe(err.Error(), false)
}

// zosiaSecret has Zosia make a secret, approved by the user in the browser, and puts it into
// the CI. It returns the secret, for asking the plan with ("" when none was made); whether a
// secret is in place or in the user's hands afterwards; and the exit code.
//
// The secret is handed to gh on its standard input. It is printed only to a terminal, and only
// when gh cannot store it.
func (s setupRun) zosiaSecret(root string, p *plan.Plan, repo string, has, known, terminal bool, o zosiaOpts, in *bufio.Reader) (string, bool, int) {
	out := s.stdout
	yes := func(line string) bool { l := strings.ToLower(line); return l == "y" || l == "yes" }
	switch {
	case !terminal && !known:
		fmt.Fprintln(s.stderr, "shearbolt zosia: --connect needs somewhere to store the secret it makes, and without a terminal that is only the GitHub CLI (gh): in a repository with GitHub workflows alone, logged in to an account that may set the repository's secrets. Nothing was connected. Run `shearbolt zosia enable` in a terminal instead; there the secret is shown to you once.")
		return "", false, 2
	case terminal && known && has:
		fmt.Fprintf(out, "\nZOSIA_SECRET is already set for %s. Replace it? [y/N] ", termSafe(repo, false))
		if line, _ := readLine(in); !yes(line) {
			fmt.Fprintln(out, "Kept the existing ZOSIA_SECRET. Shearbolt cannot read it, so your plan's room for projects in Zosia was not checked.")
			return "", true, 0
		}
	case terminal && !known:
		// Whether the CI has a secret already cannot be told here, and every yes makes a new one.
		fmt.Fprint(out, "\nMake a secret for this repository in Zosia now? [Y/n] ")
		if line, err := readLine(in); err != nil || line != "" && !yes(line) {
			return "", false, 0
		}
	}
	host, path := remoteName(s.d.gitRemote(root))
	ask := zosia.ConnectAsk{Hostname: s.d.hostname(), Version: s.d.version, SecretName: o.secretName}
	if host != "" {
		ask.Repository = host + "/" + path
	}
	if ask.SecretName == "" {
		ask.SecretName = cmp(path, repo, filepath.Base(root))
		if r := []rune(ask.SecretName); len(r) > maxSecretName {
			ask.SecretName = string(r[:maxSecretName])
		}
		for terminal {
			fmt.Fprintf(out, "\nName of the secret in Zosia [%s]: ", termSafe(ask.SecretName, false))
			line, _ := readLine(in)
			if len([]rune(line)) > maxSecretName {
				fmt.Fprintf(out, "At most %d characters.\n", maxSecretName)
				continue
			}
			if line != "" {
				ask.SecretName = line
			}
			break
		}
	}
	url, problem := s.zosiaTarget()
	if problem != "" {
		fmt.Fprintln(out, "\nNo secret was made: "+problem+".")
		if !terminal {
			return "", false, 2
		}
		return "", false, 0
	}
	if url != zosia.DefaultURL {
		fmt.Fprintf(out, "\nZOSIA_URL is set: this connects to %s, not %s.\n", termSafe(hostOf(url), false), zosia.DefaultURL)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	secret, made, err := s.d.connectZosia(ctx, url, "shearbolt/"+s.d.version, ask, func(r zosia.ConnectRequest) {
		fmt.Fprintf(out, "\nOpen this link where you are logged in to Zosia, and approve the code %s:\n\n  %s\n\nWaiting for the approval, up to %d minutes. Ctrl-C skips this.\n", r.UserCode, r.Link(url), r.ExpiresIn/60)
	})
	if err != nil {
		said := "No secret was made" + zosiaSaid(err)
		if !terminal {
			fmt.Fprintln(s.stderr, "shearbolt zosia: "+said)
			return "", false, 2
		}
		fmt.Fprintln(out, "\n"+said)
		return "", false, 0
	}
	fmt.Fprintf(out, "\nZosia made the secret %q.\n", termSafe(made, false))
	if has {
		fmt.Fprintln(out, "The secret that was there is replaced. It stays valid until you revoke it at https://zosiasec.com/app/secrets, and counts toward your plan's secrets until then.")
	}
	if known {
		err := s.d.gh(root, strings.NewReader(secret), out, s.stderr, "secret", "set", "ZOSIA_SECRET", "--repo", repo)
		if err == nil {
			fmt.Fprintf(out, "ZOSIA_SECRET is set for %s.\n", termSafe(repo, false))
			return secret, true, 0
		}
		fmt.Fprintf(out, "ZOSIA_SECRET was not set (gh: %v).\n", err)
		if !terminal {
			fmt.Fprintf(s.stderr, "shearbolt zosia: the secret %q was made in Zosia and could not be stored. Revoke it at https://zosiasec.com/app/secrets, then run this again.\n", termSafe(made, false))
			return "", false, 2
		}
	}
	fmt.Fprintf(out, "It is shown this once:\n\n  %s\n\n", secret)
	var systems []string
	for _, c := range p.CI {
		if !slices.Contains(systems, c.System) {
			systems = append(systems, c.System)
			fmt.Fprintln(out, zosiaStoreAs[c.System])
		}
	}
	return secret, true, 0
}

const maxSecretName = 64

// Where a secret that was shown goes, per CI system.
var zosiaStoreAs = map[string]string{
	"github":    "Store it as the repository secret ZOSIA_SECRET (Settings → Secrets and variables → Actions), or run `gh secret set ZOSIA_SECRET` and paste it.",
	"gitlab":    "Store it as a masked CI/CD variable ZOSIA_SECRET (Settings → CI/CD → Variables).",
	"bitbucket": "Store it as a secured repository variable ZOSIA_SECRET (Repository settings → Repository variables).",
}

// cmp returns the first of its arguments that is not empty.
func cmp(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

var (
	scpRemote  = regexp.MustCompile(`^(?:[^@/\s]+@)?([^:/\s]+):([^\s]+)$`)
	remoteHost = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)
	// Two or more segments of the characters a repository's path is made of. What a remote without a
	// scheme may carry after its first colon (a password and a host, a query) holds others.
	remotePath = regexp.MustCompile(`^[A-Za-z0-9._~-]+(?:/[A-Za-z0-9._~-]+)+$`)
)

// remoteName is the host name and the path of a git remote's URL, as a CI names the repository
// in its reports, or two empty strings when the URL is not that of a repository on a host.
// Nothing else of the URL comes back: a remote may carry a user name and a token.
func remoteName(remote string) (host, path string) {
	remote = strings.TrimSpace(remote)
	switch {
	case strings.Contains(remote, "://"):
		u, err := neturl.Parse(remote)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "ssh" && u.Scheme != "git") {
			return "", ""
		}
		host, path = u.Hostname(), u.Path
	default:
		m := scpRemote.FindStringSubmatch(remote)
		if m == nil {
			return "", ""
		}
		host, path = m[1], m[2]
	}
	host = strings.ToLower(host)
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if !remoteHost.MatchString(host) || !remotePath.MatchString(path) {
		return "", ""
	}
	return host, path
}

func countOf(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + one + "s"
}

// zosiaProjects decides which of the repository's projects send to Zosia: as the flags say, or,
// when the plan has no room for all that send, as the user selects. Without a secret there is no
// plan to ask, and only the flags count.
func (s setupRun) zosiaProjects(root string, o zosiaOpts, secret string, connected, terminal bool, in *bufio.Reader) int {
	if secret == "" && o.projects == nil && !o.all {
		return 0
	}
	out := s.stdout
	fail := func(msg string) int {
		fmt.Fprintln(s.stderr, "shearbolt zosia: "+termSafe(msg, false))
		return 2
	}
	projects, _, err := setup.ZosiaProjects(root)
	if err != nil {
		return fail(err.Error())
	}
	host, path := remoteName(s.d.gitRemote(root))
	var answer *zosia.PlanAnswer
	if url, problem := s.zosiaTarget(); secret != "" && problem != "" {
		fmt.Fprintf(out, "\nYour plan's room for projects in Zosia was not checked: %s.\n", problem)
	} else if secret != "" {
		// When this run connected, that was said there.
		if url != zosia.DefaultURL && !connected {
			fmt.Fprintf(out, "\nZOSIA_URL is set: this asks %s, not %s.\n", termSafe(hostOf(url), false), zosia.DefaultURL)
		}
		r := zosia.PlanRequest{URL: url, Secret: secret, Client: "shearbolt", Version: s.d.version}
		if host != "" {
			r.Server, r.Repository = "https://"+host, path
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		a, err := s.d.zosiaPlan(ctx, r)
		cancel()
		if err != nil {
			fmt.Fprintf(out, "\nYour plan's room for projects in Zosia was not checked%s\n", zosiaSaid(err))
		} else {
			answer = &a
		}
	}
	// room is how many more projects the plan takes; -1 when it has no limit or was not asked.
	room := -1
	if answer != nil && answer.MaxProjects != nil {
		room = max(0, *answer.MaxProjects-answer.Projects)
	}
	// A project Zosia has needs no further place, and neither does one whose gates cannot send.
	places := func(send func(setup.ZosiaProject) bool) (total, need int) {
		for _, pr := range projects {
			if send(pr) && (pr.Sends || pr.Off) {
				total++
				if answer == nil || !slices.Contains(answer.Here, pr.Config) {
					need++
				}
			}
		}
		return
	}
	noRoom := func(total, need int) string {
		has := "none"
		if room > 0 {
			has = strconv.Itoa(room)
		}
		return fmt.Sprintf("That is %s, of which %d need a place, and the plan has room for %s.", countOf(total, "project"), need, has)
	}
	write := func(sending []string) int {
		p := setup.ZosiaSend(root, sending)
		if p.Error != nil {
			return fail(p.Error.Message)
		}
		if total, need := places(func(pr setup.ZosiaProject) bool { return slices.Contains(sending, pr.Config) }); room >= 0 && need > room {
			return fail(noRoom(total, need) + " No project was switched.")
		}
		if len(p.Changes) > 0 {
			if err := setup.Write(root, p); err != nil {
				return fail(err.Error())
			}
		}
		s.printChanges(p)
		s.printNotes(p)
		if len(p.Changes) > 0 {
			fmt.Fprintf(out, "\n%s\n", zosiaReview)
		}
		return 0
	}

	switch {
	case o.all:
		var all []string
		for _, pr := range projects {
			all = append(all, pr.Config)
		}
		room = -1 // Asked for outright: Zosia refuses what it has no place for.
		return write(all)
	case o.projects != nil:
		for _, pr := range projects {
			if pr.Sends && pr.Fixed != "" && !slices.Contains(o.projects, pr.Config) {
				return fail(fmt.Sprintf("%s cannot be switched off (%s), so it has to be among --projects.", pr.Config, pr.Fixed))
			}
		}
		return write(o.projects)
	}

	fixed := func(pr setup.ZosiaProject) bool { return pr.Sends && pr.Fixed != "" }
	if _, need := places(func(pr setup.ZosiaProject) bool { return pr.Sends }); room < 0 || need <= room {
		return 0
	}
	var list []setup.ZosiaProject
	sends, known := 0, false
	for _, pr := range projects {
		if pr.Sends || pr.Off {
			list = append(list, pr)
			known = known || slices.Contains(answer.Here, pr.Config)
		}
		if pr.Sends {
			sends++
		}
	}
	// No answer could fit when the plan is full and holds none of these projects, and none when the
	// projects that cannot be switched off need more than there is: then there is nothing to select.
	if _, need := places(fixed); room == 0 && !known || need > room {
		whose := "none by this repository. Its projects' reports are"
		if len(list) == 1 {
			whose = "none by this project. Its reports are"
		}
		full := "and all of them are in use, " + whose
		if room > 0 {
			full = fmt.Sprintf("and the projects of this repository that cannot be switched off need more than the %d left. Their reports are", room)
		}
		fmt.Fprintf(out, "\nYour %s plan in Zosia allows %s, %s refused until you remove a project in Zosia or change your plan.\n",
			termSafe(answer.Plan, false), countOf(*answer.MaxProjects, "project"), full)
		return 0
	}
	elsewhere := ""
	switch others := answer.Projects - len(answer.Here); {
	case others == 1:
		elsewhere = " (1 is in use by another repository)"
	case others > 1:
		elsewhere = fmt.Sprintf(" (%d are in use by other repositories)", others)
	}
	fmt.Fprintf(out, "\nThis repository has %s that send to Zosia, and your %s plan allows %s%s. Select the projects that send their reports.\n",
		countOf(sends, "project"), termSafe(answer.Plan, false), countOf(*answer.MaxProjects, "project"), elsewhere)
	if !answer.Known && host != "" {
		fmt.Fprintf(out, "Zosia has no projects of %s/%s yet.\n", host, path)
	}
	for i, pr := range list {
		mark := ""
		switch {
		case fixed(pr):
			mark = "  (always sends: " + pr.Fixed + ")"
		case slices.Contains(answer.Here, pr.Config):
			mark = "  (in Zosia already)"
		case pr.Off:
			mark = "  (does not send now)"
		}
		fmt.Fprintf(out, "  %d. %s  %s%s\n", i+1, termSafe(pr.Name, false), termSafe(pr.Config, false), termSafe(mark, false))
	}
	if !terminal {
		fmt.Fprintln(out, "Run `shearbolt zosia enable --projects <path>[,<path>...]` with the paths of the projects that send.")
		return 3
	}
	for {
		fmt.Fprint(out, "Numbers of the projects that send, separated by commas (nothing: decide later): ")
		line, err := readLine(in)
		if line == "" {
			if err != nil {
				fmt.Fprintln(out)
			}
			fmt.Fprintln(out, "Nothing was changed: every project goes on sending, and Zosia refuses the reports of those it has no place for.")
			return 0
		}
		var chosen []string
		valid := true
		for _, f := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' }) {
			n, err := strconv.Atoi(f)
			if err != nil || n < 1 || n > len(list) {
				valid = false
				break
			}
			chosen = append(chosen, list[n-1].Config)
		}
		if !valid {
			fmt.Fprintln(out, "Give numbers from the list, such as 1,3.")
			continue
		}
		// What cannot be switched off sends whatever is chosen, and a project that is not on the list
		// is left as it is.
		for _, pr := range projects {
			if (fixed(pr) || !pr.Sends && !pr.Off) && !slices.Contains(chosen, pr.Config) {
				chosen = append(chosen, pr.Config)
			}
		}
		if total, need := places(func(pr setup.ZosiaProject) bool { return slices.Contains(chosen, pr.Config) }); need > room {
			fmt.Fprintln(out, noRoom(total, need))
			continue
		}
		return write(chosen)
	}
}
