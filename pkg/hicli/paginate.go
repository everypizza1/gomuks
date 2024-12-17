// Copyright (c) 2024 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package hicli

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/gomuks/pkg/hicli/database"
)

var ErrPaginationAlreadyInProgress = errors.New("pagination is already in progress")

/*func (h *HiClient) GetEventsByRowIDs(ctx context.Context, rowIDs []database.EventRowID) ([]*database.Event, error) {
	events, err := h.DB.Event.GetByRowIDs(ctx, rowIDs...)
	if err != nil {
		return nil, err
	} else if len(events) == 0 {
		return events, nil
	}
	firstRoomID := events[0].RoomID
	allInSameRoom := true
	for _, evt := range events {
		h.ReprocessExistingEvent(ctx, evt)
		if evt.RoomID != firstRoomID {
			allInSameRoom = false
			break
		}
	}
	if allInSameRoom {
		err = h.DB.Event.FillLastEditRowIDs(ctx, firstRoomID, events)
		if err != nil {
			return events, fmt.Errorf("failed to fill last edit row IDs: %w", err)
		}
		err = h.DB.Event.FillReactionCounts(ctx, firstRoomID, events)
		if err != nil {
			return events, fmt.Errorf("failed to fill reaction counts: %w", err)
		}
	} else {
		// TODO slow path where events are collected and filling is done one room at a time?
	}
	return events, nil
}*/

func (h *HiClient) GetEvent(ctx context.Context, roomID id.RoomID, eventID id.EventID) (*database.Event, error) {
	if evt, err := h.DB.Event.GetByID(ctx, eventID); err != nil {
		return nil, fmt.Errorf("failed to get event from database: %w", err)
	} else if evt != nil {
		h.ReprocessExistingEvent(ctx, evt)
		return evt, nil
	} else if serverEvt, err := h.Client.GetEvent(ctx, roomID, eventID); err != nil {
		return nil, fmt.Errorf("failed to get event from server: %w", err)
	} else {
		return h.processEvent(ctx, serverEvt, nil, nil, false)
	}
}

func (h *HiClient) processGetRoomState(ctx context.Context, roomID id.RoomID, fetchMembers, refetch, dispatchEvt bool) error {
	var evts []*event.Event
	if refetch {
		resp, err := h.Client.StateAsArray(ctx, roomID)
		if err != nil {
			return fmt.Errorf("failed to refetch state: %w", err)
		}
		evts = resp
	} else if fetchMembers {
		resp, err := h.Client.Members(ctx, roomID)
		if err != nil {
			return fmt.Errorf("failed to fetch members: %w", err)
		}
		evts = resp.Chunk
	}
	if evts == nil {
		return nil
	}
	dbEvts := make([]*database.Event, len(evts))
	currentStateEntries := make([]*database.CurrentStateEntry, len(evts))
	mediaReferenceEntries := make([]*database.MediaReference, len(evts))
	mediaCacheEntries := make([]*database.PlainMedia, 0, len(evts))
	for i, evt := range evts {
		dbEvts[i] = database.MautrixToEvent(evt)
		currentStateEntries[i] = &database.CurrentStateEntry{
			EventType: evt.Type,
			StateKey:  *evt.StateKey,
		}
		var mediaURL string
		if evt.Type == event.StateMember {
			currentStateEntries[i].Membership = event.Membership(evt.Content.Raw["membership"].(string))
			mediaURL, _ = evt.Content.Raw["avatar_url"].(string)
		} else if evt.Type == event.StateRoomAvatar {
			mediaURL, _ = evt.Content.Raw["url"].(string)
		}
		if mxc := id.ContentURIString(mediaURL).ParseOrIgnore(); mxc.IsValid() {
			mediaCacheEntries = append(mediaCacheEntries, (*database.PlainMedia)(&mxc))
			mediaReferenceEntries[i] = &database.MediaReference{
				MediaMXC: mxc,
			}
		}
	}
	return h.DB.DoTxn(ctx, nil, func(ctx context.Context) error {
		room, err := h.DB.Room.Get(ctx, roomID)
		if err != nil {
			return fmt.Errorf("failed to get room from database: %w", err)
		}
		updatedRoom := &database.Room{
			ID:            room.ID,
			HasMemberList: true,
		}
		err = h.DB.Event.MassUpsertState(ctx, dbEvts)
		if err != nil {
			return fmt.Errorf("failed to save events: %w", err)
		}
		for i := range currentStateEntries {
			currentStateEntries[i].EventRowID = dbEvts[i].RowID
			if mediaReferenceEntries[i] != nil {
				mediaReferenceEntries[i].EventRowID = dbEvts[i].RowID
			}
			if evts[i].Type != event.StateMember {
				processImportantEvent(ctx, evts[i], room, updatedRoom)
			}
		}
		err = h.DB.Media.AddMany(ctx, mediaCacheEntries)
		if err != nil {
			return fmt.Errorf("failed to save media cache entries: %w", err)
		}
		mediaReferenceEntries = slices.DeleteFunc(mediaReferenceEntries, func(reference *database.MediaReference) bool {
			return reference == nil
		})
		err = h.DB.Media.AddManyReferences(ctx, mediaReferenceEntries)
		if err != nil {
			return fmt.Errorf("failed to save media reference entries: %w", err)
		}
		err = h.DB.CurrentState.AddMany(ctx, room.ID, refetch, currentStateEntries)
		if err != nil {
			return fmt.Errorf("failed to save current state entries: %w", err)
		}
		roomChanged := updatedRoom.CheckChangesAndCopyInto(room)
		if roomChanged {
			err = h.DB.Room.Upsert(ctx, updatedRoom)
			if err != nil {
				return fmt.Errorf("failed to save room data: %w", err)
			}
			if dispatchEvt {
				h.EventHandler(&SyncComplete{
					Rooms: map[id.RoomID]*SyncRoom{
						roomID: {
							Meta:          room,
							Timeline:      make([]database.TimelineRowTuple, 0),
							State:         make(map[event.Type]map[string]database.EventRowID),
							AccountData:   make(map[event.Type]*database.AccountData),
							Events:        make([]*database.Event, 0),
							Reset:         false,
							Notifications: make([]SyncNotification, 0),
						},
					},
					AccountData: make(map[event.Type]*database.AccountData),
					LeftRooms:   make([]id.RoomID, 0),
				})
			}
		}
		return nil
	})
}

func (h *HiClient) GetRoomState(ctx context.Context, roomID id.RoomID, includeMembers, fetchMembers, refetch bool) ([]*database.Event, error) {
	if fetchMembers || refetch {
		if !includeMembers {
			go func(ctx context.Context) {
				err := h.processGetRoomState(ctx, roomID, fetchMembers, refetch, true)
				if err != nil {
					zerolog.Ctx(ctx).Err(err).Msg("Failed to fetch room state in background")
				}
			}(context.WithoutCancel(ctx))
		} else {
			err := h.processGetRoomState(ctx, roomID, fetchMembers, refetch, false)
			if err != nil {
				return nil, err
			}
		}
	}
	if !includeMembers {
		return h.DB.CurrentState.GetAllExceptMembers(ctx, roomID)
	}
	return h.DB.CurrentState.GetAll(ctx, roomID)
}

type PaginationResponse struct {
	Events  []*database.Event `json:"events"`
	HasMore bool              `json:"has_more"`
}

func (h *HiClient) Paginate(ctx context.Context, roomID id.RoomID, maxTimelineID database.TimelineRowID, limit int) (*PaginationResponse, error) {
	evts, err := h.DB.Timeline.Get(ctx, roomID, limit, maxTimelineID)
	if err != nil {
		return nil, err
	} else if len(evts) > 0 {
		for _, evt := range evts {
			h.ReprocessExistingEvent(ctx, evt)
		}
		return &PaginationResponse{Events: evts, HasMore: true}, nil
	} else {
		return h.PaginateServer(ctx, roomID, limit)
	}
}

func (h *HiClient) PaginateServer(ctx context.Context, roomID id.RoomID, limit int) (*PaginationResponse, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	h.paginationInterrupterLock.Lock()
	if _, alreadyPaginating := h.paginationInterrupter[roomID]; alreadyPaginating {
		h.paginationInterrupterLock.Unlock()
		return nil, ErrPaginationAlreadyInProgress
	}
	h.paginationInterrupter[roomID] = cancel
	h.paginationInterrupterLock.Unlock()
	defer func() {
		h.paginationInterrupterLock.Lock()
		delete(h.paginationInterrupter, roomID)
		h.paginationInterrupterLock.Unlock()
	}()

	room, err := h.DB.Room.Get(ctx, roomID)
	if err != nil {
		return nil, fmt.Errorf("failed to get room from database: %w", err)
	} else if room.PrevBatch == database.PrevBatchPaginationComplete {
		return &PaginationResponse{Events: []*database.Event{}, HasMore: false}, nil
	}
	resp, err := h.Client.Messages(ctx, roomID, room.PrevBatch, "", mautrix.DirectionBackward, nil, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get messages from server: %w", err)
	}
	events := make([]*database.Event, len(resp.Chunk))
	if resp.End == "" {
		resp.End = database.PrevBatchPaginationComplete
	}
	if resp.End == database.PrevBatchPaginationComplete || len(resp.Chunk) == 0 {
		err = h.DB.Room.SetPrevBatch(ctx, room.ID, resp.End)
		if err != nil {
			return nil, fmt.Errorf("failed to set prev_batch: %w", err)
		}
		return &PaginationResponse{Events: events, HasMore: resp.End != ""}, nil
	}
	wakeupSessionRequests := false
	err = h.DB.DoTxn(ctx, nil, func(ctx context.Context) error {
		if err = ctx.Err(); err != nil {
			return err
		}
		eventRowIDs := make([]database.EventRowID, len(resp.Chunk))
		decryptionQueue := make(map[id.SessionID]*database.SessionRequest)
		iOffset := 0
		for i, evt := range resp.Chunk {
			dbEvt, err := h.processEvent(ctx, evt, room.LazyLoadSummary, decryptionQueue, true)
			if err != nil {
				return err
			} else if exists, err := h.DB.Timeline.Has(ctx, roomID, dbEvt.RowID); err != nil {
				return fmt.Errorf("failed to check if event exists in timeline: %w", err)
			} else if exists {
				zerolog.Ctx(ctx).Warn().
					Int64("row_id", int64(dbEvt.RowID)).
					Str("event_id", dbEvt.ID.String()).
					Msg("Event already exists in timeline, skipping")
				iOffset++
				continue
			}
			events[i-iOffset] = dbEvt
			eventRowIDs[i-iOffset] = events[i-iOffset].RowID
		}
		if iOffset >= len(events) {
			events = events[:0]
			return nil
		}
		events = events[:len(events)-iOffset]
		eventRowIDs = eventRowIDs[:len(eventRowIDs)-iOffset]
		wakeupSessionRequests = len(decryptionQueue) > 0
		for _, entry := range decryptionQueue {
			err = h.DB.SessionRequest.Put(ctx, entry)
			if err != nil {
				return fmt.Errorf("failed to save session request for %s: %w", entry.SessionID, err)
			}
		}
		err = h.DB.Event.FillReactionCounts(ctx, roomID, events)
		if err != nil {
			return fmt.Errorf("failed to fill last edit row IDs: %w", err)
		}
		err = h.DB.Event.FillLastEditRowIDs(ctx, roomID, events)
		if err != nil {
			return fmt.Errorf("failed to fill last edit row IDs: %w", err)
		}
		err = h.DB.Room.SetPrevBatch(ctx, room.ID, resp.End)
		if err != nil {
			return fmt.Errorf("failed to set prev_batch: %w", err)
		}
		var tuples []database.TimelineRowTuple
		tuples, err = h.DB.Timeline.Prepend(ctx, room.ID, eventRowIDs)
		if err != nil {
			return fmt.Errorf("failed to prepend events to timeline: %w", err)
		}
		for i, evt := range events {
			evt.TimelineRowID = tuples[i].Timeline
		}
		return nil
	})
	if err == nil && wakeupSessionRequests {
		h.WakeupRequestQueue()
	}
	return &PaginationResponse{Events: events, HasMore: true}, err
}
