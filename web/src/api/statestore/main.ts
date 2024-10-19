// gomuks - A Matrix client written in Go.
// Copyright (C) 2024 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.
import unhomoglyph from "unhomoglyph"
import { getMediaURL } from "@/api/media.ts"
import { NonNullCachedEventDispatcher } from "@/util/eventdispatcher.ts"
import { focused } from "@/util/focus.ts"
import type {
	ContentURI, EventRowID,
	EventsDecryptedData,
	MemDBEvent,
	RoomID,
	SendCompleteData,
	SyncCompleteData,
	SyncRoom,
} from "../types"
import { RoomStateStore } from "./room.ts"

export interface RoomListEntry {
	room_id: RoomID
	sorting_timestamp: number
	preview_event?: MemDBEvent
	preview_sender?: MemDBEvent
	name: string
	search_name: string
	avatar?: ContentURI
	unread_messages: number
	unread_notifications: number
	unread_highlights: number
}

// eslint-disable-next-line no-misleading-character-class
const removeHiddenCharsRegex = /[\u2000-\u200F\u202A-\u202F\u0300-\u036F\uFEFF\u061C\u2800\u2062-\u2063\s]/g

export function toSearchableString(str: string): string {
	return unhomoglyph(str.normalize("NFD").replace(removeHiddenCharsRegex, ""))
		.toLowerCase()
		.replace(/[\\'!"#$%&()*+,\-./:;<=>?@[\]^_`{|}~\u2000-\u206f\u2e00-\u2e7f]/g, "")
		.toLowerCase()
}

export class StateStore {
	readonly rooms: Map<RoomID, RoomStateStore> = new Map()
	readonly roomList = new NonNullCachedEventDispatcher<RoomListEntry[]>([])
	switchRoom?: (roomID: RoomID) => void
	imageAuthToken?: string

	#roomListEntryChanged(entry: SyncRoom, oldEntry: RoomStateStore): boolean {
		return entry.meta.sorting_timestamp !== oldEntry.meta.current.sorting_timestamp ||
			entry.meta.unread_messages !== oldEntry.meta.current.unread_messages ||
			entry.meta.unread_notifications !== oldEntry.meta.current.unread_notifications ||
			entry.meta.unread_highlights !== oldEntry.meta.current.unread_highlights ||
			entry.meta.preview_event_rowid !== oldEntry.meta.current.preview_event_rowid ||
			entry.events.findIndex(evt => evt.rowid === entry.meta.preview_event_rowid) !== -1
	}

	#makeRoomListEntry(entry: SyncRoom, room?: RoomStateStore): RoomListEntry {
		if (!room) {
			room = this.rooms.get(entry.meta.room_id)
		}
		const preview_event = room?.eventsByRowID.get(entry.meta.preview_event_rowid)
		const preview_sender = preview_event && room?.getStateEvent("m.room.member", preview_event.sender)
		const name = entry.meta.name ?? "Unnamed room"
		return {
			room_id: entry.meta.room_id,
			sorting_timestamp: entry.meta.sorting_timestamp,
			preview_event,
			preview_sender,
			name,
			search_name: toSearchableString(name),
			avatar: entry.meta.avatar,
			unread_messages: entry.meta.unread_messages,
			unread_notifications: entry.meta.unread_notifications,
			unread_highlights: entry.meta.unread_highlights,
		}
	}

	applySync(sync: SyncCompleteData) {
		const resyncRoomList = this.roomList.current.length === 0
		const changedRoomListEntries = new Map<RoomID, RoomListEntry>()
		for (const [roomID, data] of Object.entries(sync.rooms)) {
			let isNewRoom = false
			let room = this.rooms.get(roomID)
			if (!room) {
				room = new RoomStateStore(data.meta)
				this.rooms.set(roomID, room)
				isNewRoom = true
			}
			const roomListEntryChanged = !resyncRoomList && (isNewRoom || this.#roomListEntryChanged(data, room))
			room.applySync(data)
			if (roomListEntryChanged) {
				changedRoomListEntries.set(roomID, this.#makeRoomListEntry(data, room))
			}

			if (Notification.permission === "granted" && !focused.current) {
				for (const notification of data.notifications) {
					this.showNotification(room, notification.event_rowid, notification.sound)
				}
			}
		}

		let updatedRoomList: RoomListEntry[] | undefined
		if (resyncRoomList) {
			updatedRoomList = Object.values(sync.rooms).map(entry => this.#makeRoomListEntry(entry))
			updatedRoomList.sort((r1, r2) => r1.sorting_timestamp - r2.sorting_timestamp)
		} else if (changedRoomListEntries.size > 0) {
			updatedRoomList = this.roomList.current.filter(entry => !changedRoomListEntries.has(entry.room_id))
			for (const entry of changedRoomListEntries.values()) {
				if (updatedRoomList.length === 0 || entry.sorting_timestamp >=
					updatedRoomList[updatedRoomList.length - 1].sorting_timestamp) {
					updatedRoomList.push(entry)
				} else if (entry.sorting_timestamp <= 0 ||
					entry.sorting_timestamp < updatedRoomList[0]?.sorting_timestamp) {
					updatedRoomList.unshift(entry)
				} else {
					const indexToPushAt = updatedRoomList.findLastIndex(val =>
						val.sorting_timestamp <= entry.sorting_timestamp)
					updatedRoomList.splice(indexToPushAt + 1, 0, entry)
				}
			}
		}
		if (updatedRoomList) {
			this.roomList.emit(updatedRoomList)
		}
	}

	showNotification(room: RoomStateStore, rowid: EventRowID, sound: boolean) {
		const evt = room.eventsByRowID.get(rowid)
		if (!evt || typeof evt.content.body !== "string") {
			return
		}
		let body = evt.content.body
		if (body.length > 400) {
			body = body.slice(0, 350) + " […]"
		}
		const memberEvt = room.getStateEvent("m.room.member", evt.sender)
		const icon = `${getMediaURL(memberEvt?.content.avatar_url)}&image_auth=${this.imageAuthToken}`
		const roomName = room.meta.current.name ?? "Unnamed room"
		const senderName = memberEvt?.content.displayname ?? evt.sender
		const title = senderName === roomName ? senderName : `${senderName} (${roomName})`
		const notif = new Notification(title, {
			body,
			icon,
			badge: "/gomuks.png",
			// timestamp: evt.timestamp,
			// image: ...,
			tag: rowid.toString(),
		})
		notif.onclick = () => this.onClickNotification(room.roomID)
		if (sound) {
			// TODO play sound
		}
	}

	onClickNotification(roomID: RoomID) {
		if (this.switchRoom) {
			this.switchRoom(roomID)
		}
	}

	applySendComplete(data: SendCompleteData) {
		const room = this.rooms.get(data.event.room_id)
		if (!room) {
			// TODO log or something?
			return
		}
		room.applySendComplete(data.event)
	}

	applyDecrypted(decrypted: EventsDecryptedData) {
		const room = this.rooms.get(decrypted.room_id)
		if (!room) {
			// TODO log or something?
			return
		}
		room.applyDecrypted(decrypted)
		if (decrypted.preview_event_rowid) {
			const idx = this.roomList.current.findIndex(entry => entry.room_id === decrypted.room_id)
			if (idx !== -1) {
				const updatedRoomList = [...this.roomList.current]
				updatedRoomList[idx] = {
					...updatedRoomList[idx],
					preview_event: room.eventsByRowID.get(decrypted.preview_event_rowid),
				}
				this.roomList.emit(updatedRoomList)
			}
		}
	}
}