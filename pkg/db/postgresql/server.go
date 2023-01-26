package postgresql

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/migalabs/armiarma/pkg/db/models"
	eth "github.com/migalabs/armiarma/pkg/networks/ethereum"
	"github.com/migalabs/armiarma/pkg/utils"
	log "github.com/sirupsen/logrus"

	pgx "github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/pkg/errors"
)

const (
	batchFlushingTimeout = 1 * time.Second
	batchSize            = 256
	maxPersisters        = 1
)

var (
	noQueryError  string = "no error"
	noQueryResult string = "no result"
)

type DBClient struct {
	// Control Variables
	ctx context.Context

	// Network that we are Crawling
	Network utils.NetworkType

	// Pgx Postgres variables
	loginStr string
	psqlPool *pgxpool.Pool

	// Request channels
	persistC chan interface{}
	doneC    chan struct{}
	wg       *sync.WaitGroup
}

func NewDBClient(
	ctx context.Context,
	p2pNetwork utils.NetworkType,
	loginStr string,
	initialized bool) (*DBClient, error) {
	// check if the login string has enough len
	if len(loginStr) == 0 {
		return nil, errors.New("empty db-endpoint provided")
	}

	// try connecting to the DB from the given logingStr
	pPool, err := pgxpool.Connect(ctx, loginStr)
	if err != nil {
		return nil, err
	}
	log.WithFields(log.Fields{"endpoint": loginStr}).Debug("successful connection to DB")

	// check if the connection is successful
	err = pPool.Ping(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "unable to ping db")
	}

	// generate all the necessary/control channels
	persistC := make(chan interface{}, batchSize)

	var wg sync.WaitGroup

	// compose the DBClient
	dbClient := &DBClient{
		ctx:      ctx,
		Network:  p2pNetwork,
		loginStr: loginStr,
		psqlPool: pPool,
		persistC: persistC,
		doneC:    make(chan struct{}),
		wg:       &wg,
	}

	// initialize all the tables
	if initialized {
		err = dbClient.initTables()
		if err != nil {
			return nil, errors.Wrap(err, "unable to initialize the SQL tables at "+loginStr)
		}
	}

	// run the db persisters
	go dbClient.launchPersister()

	return dbClient, nil
}

func (c *DBClient) initTables() error {
	// initialize all the necesary tables to perform the crawl

	var err error

	// peer_info table
	err = c.InitPeerInfoTable()
	if err != nil {
		return errors.Wrap(err, "initializing peer_info table")
	}

	// conn_event
	err = c.InitConnEventTable()
	if err != nil {
		return errors.Wrap(err, "initializing conn_events table")
	}

	// ip table
	err = c.InitIpTable()
	if err != nil {
		return errors.Wrap(err, "initializing ips table")
	}

	// eth_nodes table
	err = c.InitEthNodesTable()
	if err != nil {
		return errors.Wrap(err, "initializing eth_nodes table")
	}

	// eth_status table
	err = c.InitEthereumNodeStatus()
	if err != nil {
		return errors.Wrap(err, "initializing eth_status table")
	}

	// INIT all the tables - Separate Networks

	return err
}

func (c *DBClient) launchPersister() {
	logEntry := log.WithFields(log.Fields{
		"mod": "db-persister",
	})
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()

		// batch to aggregate all the queries
		batch := &pgx.Batch{}
		isReadyToPersistFn := func(batch *pgx.Batch) bool {
			return batch.Len() >= batchSize
		}
		batchQueryFn := func(batch *pgx.Batch, query string, args ...interface{}) {
			// fmt.Printf("adding query %s\n with args (%d) %+v\n", query, len(args), args)
			batch.Queue(query, args...)
		}
		persistBatchFn := func(batch *pgx.Batch) error {
			logEntry.Debugf("persisting batch of queries with len(%d)", batch.Len())
			t := time.Now()

			// if batch len == 0, don't even query
			if batch.Len() == 0 {
				logEntry.Debug("skipping batch-query, no queries to persist")
				return nil
			}

			// begin pgx.Tx
			tx, err := c.psqlPool.Begin(c.ctx)
			if err != nil {
				return errors.Wrap(err, "unable to persist batch")
			}
			// Add batch to TX
			batchResults := tx.SendBatch(c.ctx, batch)

			// Exec the queries
			var qerr error
			var rows pgx.Rows
			var cnt int
			for qerr == nil {
				rows, qerr = batchResults.Query()
				rows.Close()
				cnt++
				if cnt-1 > batch.Len() {
					log.Warnf("are we stuck persisting queries? %d iters out of %d queries", cnt, batch.Len())
				}
			}
			// check if there was any error
			if qerr.Error() != noQueryResult {
				return errors.Wrap(err, fmt.Sprintf("unable to persist betch because an error on row %d \n %+v\n", cnt, rows))
			}

			logEntry.Debugf("batch with %d queries successfully persisted in %s", cnt-1, time.Since(t))
			return tx.Commit(c.ctx)
		}

		// batch flushing ticker
		ticker := time.NewTicker(batchFlushingTimeout)

		var readyToFinish bool

	persistingLoop:
		for {
			if readyToFinish && len(c.persistC) == 0 {
				break persistingLoop
			}

			// check with higher priority if the main-ctx died
			select {
			case <-c.ctx.Done(): // check if the context of the tool died
				logEntry.Info("context died, clossing persister")
				readyToFinish = true
			case <-c.doneC:
				logEntry.Info("closed detected, clossing persister")
				readyToFinish = true
			default:
			}

			// load  or flush after
			select {
			case obj := <-c.persistC: // persist any kind of item
				// Every item/SQL query  has to return (string. []interfaces)
				switch obj.(type) {
				case (*models.HostInfo):
					hostInfo := obj.(*models.HostInfo)
					logEntry.Tracef("persisting host_info %s\n", hostInfo.ID.String())
					// // double-check when are we rewriting hInfo without IP, and port
					// if hostInfo.IP == "" {
					// 	log.Error("error trying to add host info without IP and ports", hostInfo)
					// }
					// add raw new HostInfo
					q, args := c.UpsertHostInfo(hostInfo)
					batchQueryFn(batch, q, args...)

					// check if the peerInfo needs to update anything else
					if hostInfo.IsHostIdentified() {
						logEntry.Tracef("host_info has peer_info %s\n", hostInfo.PeerInfo.RemotePeer.String())
						q, args = c.UpdatePeerInfo(&hostInfo.PeerInfo)
						batchQueryFn(batch, q, args...)
					}
					// Read all the Attributes in hInfo
					for attName, att := range hostInfo.Attr {
						log.Debugf("detected attribute %s on peer", attName)
						// TODO: add tables for BeaconStatus and BeaconMetadata
						switch att.(type) {
						case eth.BeaconStatusStamped:
							bstatus := att.(eth.BeaconStatusStamped)
							q, args = c.UpsertEthereumNodeStatus(bstatus)
							batchQueryFn(batch, q, args...)
						case eth.BeaconMetadataStamped:
							bmetadata := att.(eth.BeaconMetadataStamped)
							q, args = c.UpsertEthereumNodeMetadata(bmetadata)
							batchQueryFn(batch, q, args...)
						case (*eth.EnrNode):
							enrNode := att.(*eth.EnrNode)
							logEntry.Tracef("persisting eth node_info %s\n", enrNode.ID.String())
							q, args := c.UpsertEnrInfo(enrNode)
							batchQueryFn(batch, q, args...)
						default:
							log.Warnf("not yet recognized type for attr %s - %T - %+v", attName, att, att)
						}
					}

				case (*models.PeerInfo):
					peerInfo := obj.(*models.PeerInfo)
					logEntry.Tracef("persisting new peer_info %s\n", peerInfo.RemotePeer.String())
					q, args := c.UpdatePeerInfo(peerInfo)
					batchQueryFn(batch, q, args...)

				case (*models.ConnectionAttempt):
					connAttempt := obj.(*models.ConnectionAttempt)
					logEntry.Tracef("persisting conn_attempt")
					q, args := c.UpdateConnAttempt(connAttempt)

					batchQueryFn(batch, q, args...)
				case (*models.ConnEvent):
					connEvent := obj.(*models.ConnEvent)
					logEntry.Tracef("persisting conn_event for peer %s\n", connEvent.PeerID.String())
					q, args := c.InsertNewConnEvent(connEvent)
					batchQueryFn(batch, q, args...)

					// Control Info LastActivity based on last disconnection
					// get the disconnection time to update the LastActivity timestamp in the peer_info table
					q, args = c.UpdateLastActivityTimestamp(connEvent.PeerID, connEvent.DiscTime)
					batchQueryFn(batch, q, args...)

				case (models.IpInfo):
					ipInfo := obj.(models.IpInfo)
					logEntry.Tracef("persisting ip_info %s\n", ipInfo.IP)
					q, args := c.UpsertIpInfo(ipInfo)
					batchQueryFn(batch, q, args...)

				default:
					logEntry.Errorf("unrecognized type of object received to persist into DB %T", obj)
					logEntry.Error(obj)
				}

				// after adding whatever query we got check if we need to persist the batch
				if isReadyToPersistFn(batch) {
					logEntry.Debug("batch-query full, ready to persist")
					err := persistBatchFn(batch)
					if err != nil {
						log.Error(err)
					}
					// after peristing the batch, we can already flush all the
					batch = &pgx.Batch{}
				}

			case <-ticker.C:
				logEntry.Trace("ticker jumped - flushing content of query-batch")
				// flush the batched queries
				err := persistBatchFn(batch)
				if err != nil {
					log.Error(err)
				}
				// after peristing the batch, we can already flush all the
				batch = &pgx.Batch{}
			}
		}
	}()
}

func (c *DBClient) Close() {
	// Let the persister finish cleaning the batch
	c.doneC <- struct{}{}
	c.wg.Wait()

	// close safelly the connection with PSQL
	c.psqlPool.Close() // TODO: fix hanging call

	// close all the exisiting channels
	close(c.persistC)
}

func (c *DBClient) PersistToDB(persItem interface{}) {
	c.persistC <- persItem
}

func (c *DBClient) SingleQuery(query string, args ...interface{}) (interface{}, error) {
	return c.psqlPool.Exec(c.ctx, query, args...)
}
