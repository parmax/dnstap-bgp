package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/vishvananda/netns"
)

type cfgRoot struct {
	Domains   string
	Cache     string
	Namespace string
	TTL       string

	DNSTap *dnstapCfg
	BGP    *bgpCfg
	Syncer *syncerCfg
}

func main() {
	var (
		dTree   *domainTree
		bgp     *bgpServer
		ipCache *cache
		ipDB    *db
		syncer  *syncer

		err      error
		shutdown = make(chan struct{})
	)

	config := flag.String("config", "", "Path to a config file")
	flag.Parse()

	if *config == "" {
		log.Fatal("You need to specify path to a config file")
	}

	cfg := &cfgRoot{}
	if _, err = toml.DecodeFile(*config, &cfg); err != nil {
		log.Fatalf("Unable to parse config file '%s': %s", *config, err)
	}

	if cfg.Domains == "" {
		log.Fatal("You need to specify path to a domain list")
	}

	ttl := 24 * time.Hour
	if cfg.TTL != "" {
		if ttl, err = time.ParseDuration(cfg.TTL); err != nil {
			log.Printf("Unable to parse TTL: %s", err)
		}
	}

	if cfg.Namespace != "" {
		nsh, err := netns.GetFromName(cfg.Namespace)
		if err != nil {
			log.Fatalf("Unable to find namespace '%s': %s", cfg.Namespace, err)
		}

		if err = netns.Set(nsh); err != nil {
			log.Fatalf("Unable to switch to namespace '%s': %s", cfg.Namespace, err)
		}

		log.Printf("Switched to namespace '%s'", cfg.Namespace)
	}

	expireCb := func(e *cacheEntry) {
		log.Printf("%s (%s) expired", e.IP, e.Domain)
		bgp.delHost(e.IP)

		if ipDB != nil {
			ipDB.del(e.IP)
		}
	}

	ipCache = newCache(ttl, expireCb)

	dTree = newDomainTree()
	cnt, skip, err := dTree.loadFile(cfg.Domains)
	if err != nil {
		log.Fatalf("Unable to load domain list: %s", err)
	}

	log.Printf("Domains loaded: %d, skipped: %d", cnt, skip)

	if cfg.Cache != "" {
		if ipDB, err = newDB(cfg.Cache); err != nil {
			log.Fatalf("Unable to init DB '%s': %s", cfg.Cache, err)
		}

		es, err := ipDB.fetchAll()
		if err != nil {
			log.Fatalf("Unable to load entries from DB: %s", err)
		}

		now := time.Now()
		i, j, k := 0, 0, 0
		for _, e := range es {
			if now.Sub(e.TS) >= ttl {
				ipDB.del(e.IP)
				j++
				continue
			}

			if !dTree.has(e.Domain) {
				ipDB.del(e.IP)
				k++
				continue
			}

			ipCache.add(e)
			i++
		}

		log.Printf("Loaded from DB: %d, expired: %d, vanished: %d", i, j, k)
	}

	if bgp, err = newBgp(cfg.BGP); err != nil {
		log.Fatalf("Unable to init BGP: %s", err)
	}

	ipDBPut := func(e *cacheEntry) {
		if ipDB == nil {
			return
		}

		if err := ipDB.add(e); err != nil {
			log.Printf("Unable to add (%s, %s) to DB: %s", e.IP, e.Domain, err)
		}
	}

	addEntry := func(e *cacheEntry, touch bool) bool {
		if ipCache.exists(e.IP, touch) {
			if touch {
				ipDBPut(e)
			}

			return false
		}

		log.Printf("%s: %s (from peer: %t)", e.Domain, e.IP, !touch)
		bgp.addHost(e.IP)
		ipCache.add(e)
		ipDBPut(e)

		return true
	}

	if cfg.Syncer.Listen != "" || len(cfg.Syncer.Peers) > 0 {
		syncerCb := func(peer string, new int, err error) {
			log.Printf("Syncer: Peer %s: synced: %d error: %v", peer, new, err)
		}

		if syncer, err = newSyncer(cfg.Syncer, ipCache.getAll, addEntry, syncerCb); err != nil {
			log.Fatalf("Unable to init syncer: %s", err)
		}
	}

	addHostCb := func(ip, domain string) {
		if !dTree.has(domain) {
			return
		}

		e := &cacheEntry{
			IP:     ip,
			Domain: domain,
			TS:     time.Now(),
		}

		addEntry(e, true)

		if syncer != nil {
			if err := syncer.broadcast(e); err != nil {
				log.Printf("Unable to broadcast [%s %s]: %s", ip, domain, err)
			}
		}
	}

	dnsTapErrorCb := func(err error) {
		log.Println(err)
	}

	if _, err = newDnstapServer(cfg.DNSTap, addHostCb, dnsTapErrorCb); err != nil {
		log.Fatalf("Unable to init DNSTap: %s", err)
	}

	log.Printf("Created DNSTap socket %s", cfg.DNSTap.Socket)

	go func() {
		sigchannel := make(chan os.Signal, 1)
		signal.Notify(sigchannel, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGUSR1, os.Interrupt)

		for sig := range sigchannel {
			switch sig {
			case syscall.SIGHUP:
				if i, s, err := dTree.loadFile(cfg.Domains); err != nil {
					log.Printf("Unable to load file: %s", err)
				} else {
					log.Printf("Domains loaded: %d, skipped: %d", i, s)
				}

			case os.Interrupt, syscall.SIGTERM:
				close(shutdown)

			case syscall.SIGUSR1:
				log.Printf("IPs exported: %d, domains loaded: %d", ipCache.count(), dTree.count())
			}
		}
	}()

	<-shutdown
	bgp.close()

	if syncer != nil {
		syncer.close()
	}

	if ipDB != nil {
		ipDB.close()
	}
}
