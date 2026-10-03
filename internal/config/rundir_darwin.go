package config

// macOS's sealed system volume has no /run and root can't create one;
// /var/run is its writable equivalent.
const DefaultRunDir = "/var/run/nodexa"
