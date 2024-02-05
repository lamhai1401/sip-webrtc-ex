package main

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/rs/zerolog/log"
)

func setupSipProxy(proxydst string, ip string) *sipgo.Server {
	// Prepare all variables we need for our service
	host, port, _ := sip.ParseAddr(ip)
	ua, err := sipgo.NewUA(
		sipgo.WithUserAgentHostname("Beowufl"),
	)
	fmt.Println(net.ParseIP(host))
	if err != nil {
		log.Fatal().Err(err).Msg("Fail to setup user agent")
	}

	srv, err := sipgo.NewServer(ua)
	if err != nil {
		log.Fatal().Err(err).Msg("Fail to setup server handle")
	}

	client, err := sipgo.NewClient(ua) // sipgo.WithClientAddr(ip+"5060"),
	// sipgo.WithClientPort(5060),

	if err != nil {
		log.Fatal().Err(err).Msg("Fail to setup client handle")
	}

	registry := NewRegistry()
	var getDestination = func(req *sip.Request) string {
		tohead := req.To()
		dst := registry.Get(tohead.Address.User)

		if dst == "" {
			return proxydst
		}

		return dst
	}

	var reply = func(tx sip.ServerTransaction, req *sip.Request, code sip.StatusCode, reason string) {
		resp := sip.NewResponseFromRequest(req, code, reason, nil)
		resp.SetDestination(req.Source()) //This is optional, but can make sure not wrong via is read
		if err := tx.Respond(resp); err != nil {
			log.Error().Err(err).Msg("Fail to respond on transaction")
		}
	}

	var route = func(req *sip.Request, tx sip.ServerTransaction) {
		// If we are proxying to asterisk or other proxy -dst must be set
		// Otherwise we will look on our registration entries
		dst := getDestination(req)

		if dst == "" {
			reply(tx, req, 404, "Not found")
			return
		}

		// recipient := &sip.Uri{}
		// sip.ParseUri(fmt.Sprintf("sip:%s@%s", "alice", clientReq.Source()), recipient)
		// req := sip.NewRequest(sip.INVITE, recipient)
		// req.AppendHeader(
		// 	sip.NewHeader("Contact", fmt.Sprintf("<sip:%s@%s>", "alice", req.Source())),
		// )
		// req.AppendHeader(sip.NewHeader("Allow", "PRACK, INVITE, ACK, BYE, CANCEL, UPDATE, INFO, SUBSCRIBE, NOTIFY, REFER, MESSAGE, OPTIONS"))
		req.SetDestination(dst)
		// req.SetTransport(strings.ToUpper(clientReq.Transport()))
		// req.SetBody([]byte("Hello from Server"))

		ctx := context.Background()

		// req.SetDestination(dst)
		// Start client transaction and relay our request
		clTx, err := client.TransactionRequest(ctx, req, sipgo.ClientRequestAddRecordRoute)
		if err != nil {
			log.Error().Err(err).Msg("RequestWithContext  failed")
			reply(tx, req, 500, "")
			return
		}
		defer clTx.Terminate()

		// Keep monitoring transactions, and proxy client responses to server transaction
		log.Info().Str("req", req.Method.String()).Msg("Starting transaction")
		for {
			select {

			case res, more := <-clTx.Responses():
				if !more {
					return
				}

				res.SetDestination(req.Source())

				// https://datatracker.ietf.org/doc/html/rfc3261#section-16.7
				// Based on section removing via. Topmost via should be removed and check that exist

				// Removes top most header
				res.RemoveHeader("Via")
				if err := tx.Respond(res); err != nil {
					log.Error().Err(err).Msg("ResponseHandler transaction respond failed")
				}

				// Early terminate
				// if req.Method == sip.BYE {
				// 	// We will call client Terminate
				// 	return
				// }

			case m := <-tx.Acks():
				// Acks can not be send directly trough destination
				log.Info().Str("m", m.StartLine()).Str("dst", dst).Msg("Proxing ACK")
				m.SetDestination(dst)
				client.WriteRequest(m)

			case m := <-tx.Cancels():
				// Send response imediatelly
				reply(tx, m, 200, "OK")
				// Cancel client transacaction without waiting. This will send CANCEL request
				clTx.Cancel()

			case <-tx.Done():
				if err := tx.Err(); err != nil {
					log.Error().Err(err).Str("req", req.Method.String()).Msg("Transaction done with error")
					return
				}
				log.Debug().Str("req", req.Method.String()).Msg("Transaction done")
				return
			}
		}
	}

	var registerHandler = func(req *sip.Request, tx sip.ServerTransaction) {
		// https://www.rfc-editor.org/rfc/rfc3261#section-10.3
		cont := req.Contact()

		// We have a list of uris
		uri := cont.Address
		if uri.Host == host && uri.Port == port {
			reply(tx, req, 401, "Contact address not provided")
			return
		}

		addr := uri.Host + ":" + strconv.Itoa(uri.Port)

		registry.Add(uri.User, addr)
		log.Debug().Msgf("Contact added %s -> %s", uri.User, addr)

		res := sip.NewResponseFromRequest(req, 200, "OK", []byte("Hello from server"))
		// log.Debug().Msgf("Sending response: \n%s", res.String())

		// URI params must be reset or this should be regenetad
		cont.Address.UriParams = sip.NewParams()
		cont.Address.UriParams.Add("transport", req.Transport())

		if err := tx.Respond(res); err != nil {
			log.Error().Err(err).Msg("Sending REGISTER OK failed")
			return
		}

		log.Info().Str("client", addr).Msg("REGISTER OK")
		fmt.Println(req.Headers())
	}

	var inviteHandler = func(req *sip.Request, tx sip.ServerTransaction) {
		rtpListenerPort := startRTPListener()
		res := sip.NewResponseFromRequest(req, 200, "OK", generateAnswer(req.Body(), *unicastAddress, rtpListenerPort))
		res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Host: *unicastAddress, Port: *sipPort}})
		res.AppendHeader(&contentTypeHeaderSDP)
		if err = tx.Respond(res); err != nil {
			panic(err)
		}
		fmt.Printf("Accepting SIP Invite: %s\n", req.From())
	}

	var ackHandler = func(req *sip.Request, tx sip.ServerTransaction) {
		fmt.Println(tx)
		dst := getDestination(req)
		if dst == "" {
			return
		}
		req.SetDestination(dst)
		if err := client.WriteRequest(req, sipgo.ClientRequestAddVia); err != nil {
			log.Error().Err(err).Msg("Send failed")
			reply(tx, req, 500, "")
		}
		log.Info().Msg("ackHandler OK")
	}

	var cancelHandler = func(req *sip.Request, tx sip.ServerTransaction) {
		fmt.Println(tx)
		log.Info().Msg("cancelHandler OK")
		route(req, tx)
	}

	var byeHandler = func(req *sip.Request, tx sip.ServerTransaction) {
		fmt.Println(tx)
		log.Info().Msg("byeHandler OK")
		route(req, tx)
	}

	srv.OnRegister(registerHandler)
	srv.OnInvite(inviteHandler)
	srv.OnAck(ackHandler)
	srv.OnCancel(cancelHandler)
	srv.OnBye(byeHandler)
	srv.OnMessage(func(req *sip.Request, tx sip.ServerTransaction) {
		fmt.Println(tx)
		log.Info().Interface("Body", string(req.Body())).Msg("OnMessage OK")
	})
	return srv
}
