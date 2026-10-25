#!/bin/sh
# Builds the signed apt and dnf repository published on GitHub Pages.
#   sh packaging/repo/build.sh <packages dir> <output dir> <repository URL>
# The packages directory holds the .deb and .rpm files to publish. The key
# with fingerprint REPO_SIGNING_KEY_ID must be in the GnuPG keyring; its public
# key is published as linux-ha-agent.asc. Unsigned .rpm files are skipped,
# because dnf checks the signature of every package. Needs apt-ftparchive
# (apt-utils), createrepo_c, rpm and gpg.
set -eu

in=$1
out=$2
url=${3%/}
key=${REPO_SIGNING_KEY_ID:?set REPO_SIGNING_KEY_ID to the signing key fingerprint}

mkdir -p "$out/deb" "$out/rpm"
gpg --batch --armor --export "$key" >"$out/linux-ha-agent.asc"

# apt: a flat repository (no dists/), so its sources entry is "Suites: ./".
cp "$in"/*.deb "$out/deb/"
(
  cd "$out/deb"
  apt-ftparchive packages . >Packages
  gzip -9kn Packages
  apt-ftparchive \
    -o APT::FTPArchive::Release::Origin=linux-ha-agent \
    -o APT::FTPArchive::Release::Label=linux-ha-agent \
    release . >../Release.tmp # not in ., or Release would list itself
  mv ../Release.tmp Release
  gpg --batch --yes --local-user "$key" --clearsign --output InRelease Release
  gpg --batch --yes --local-user "$key" --armor --detach-sign --output Release.gpg Release
)

# dnf: only signed packages; the metadata is signed as well (repo_gpgcheck).
for rpm in "$in"/*.rpm; do
  if [ "$(rpm -qp --qf '%{RSAHEADER:pgpsig}' "$rpm" 2>/dev/null)" = "(none)" ]; then
    echo "skipping unsigned $(basename "$rpm")"
    continue
  fi
  cp "$rpm" "$out/rpm/"
done
# gzip metadata, so older dnf versions can read it too.
createrepo_c --quiet --general-compress-type=gz "$out/rpm"
gpg --batch --yes --local-user "$key" --armor --detach-sign \
  --output "$out/rpm/repodata/repomd.xml.asc" "$out/rpm/repodata/repomd.xml"

cat >"$out/linux-ha-agent.sources" <<EOF
Types: deb
URIs: $url/deb/
Suites: ./
Signed-By: /etc/apt/keyrings/linux-ha-agent.asc
EOF

cat >"$out/linux-ha-agent.repo" <<EOF
[linux-ha-agent]
name=linux-ha-agent
baseurl=$url/rpm/
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=$url/linux-ha-agent.asc
EOF

cat >"$out/index.html" <<EOF
<!doctype html>
<meta charset="utf-8">
<title>linux-ha-agent packages</title>
<h1>linux-ha-agent packages</h1>
<p>apt and dnf repository for
<a href="https://github.com/lvlie/linux-ha-agent">linux-ha-agent</a>.
Set it up with the install script:</p>
<pre>curl -fsSL https://github.com/lvlie/linux-ha-agent/releases/latest/download/install.sh | sudo sh</pre>
<p>Or by hand:</p>
<pre># Debian, Ubuntu, Proxmox VE
sudo install -d -m 0755 /etc/apt/keyrings
sudo curl -fsSL -o /etc/apt/keyrings/linux-ha-agent.asc $url/linux-ha-agent.asc
sudo curl -fsSL -o /etc/apt/sources.list.d/linux-ha-agent.sources $url/linux-ha-agent.sources
sudo apt update &amp;&amp; sudo apt install linux-ha-agent

# Fedora, RHEL, Rocky, Alma
sudo curl -fsSL -o /etc/yum.repos.d/linux-ha-agent.repo $url/linux-ha-agent.repo
sudo dnf install linux-ha-agent</pre>
<p>Signing key: <code>$key</code></p>
EOF

set -- "$out"/deb/*.deb
debs=$#
set -- "$out"/rpm/*.rpm
[ -e "$1" ] || shift
echo "repository built in $out: $debs .deb, $# .rpm"
