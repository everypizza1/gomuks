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
import { Suspense, lazy, use } from "react"
import { GridLoader } from "react-spinners"
import { usePreference } from "@/api/statestore"
import { LocationMessageEventContent } from "@/api/types"
import { GOOGLE_MAPS_API_KEY } from "@/util/keys.ts"
import { ensureString } from "@/util/validation.ts"
import ClientContext from "../../ClientContext.ts"
import EventContentProps from "./props.ts"

const GomuksLeaflet = lazy(() => import("./Leaflet.tsx"))

function parseGeoURI(uri: unknown): [lat: number, long: number, prec: number] {
	const geoURI = ensureString(uri)
	if (!geoURI.startsWith("geo:")) {
		return [0, 0, 0]
	}
	try {
		const [coordinates/*, params*/] = geoURI.slice("geo:".length).split(";")
		const [lat, long] = coordinates.split(",").map(parseFloat)
		// const decodedParams = new URLSearchParams(params)
		const prec = 13 // (+(decodedParams.get("u") ?? 0)) || 13
		return [lat, long, prec]
	} catch {
		return [0, 0, 0]
	}
}

const locationLoader = <div className="location-importer"><GridLoader color="var(--primary-color)" size={25}/></div>

const LocationMessageBody = ({ event, room }: EventContentProps) => {
	const content = event.content as LocationMessageEventContent
	const client = use(ClientContext)!
	const mapProvider = usePreference(client.store, room, "map_provider")
	const tileTemplate = usePreference(client.store, room, "leaflet_tile_template")
	const [lat, long, prec] = parseGeoURI(content.geo_uri)
	const marker = ensureString(content["org.matrix.msc3488.location"]?.description ?? content.body)
	if (mapProvider === "leaflet") {
		return <div className="location-container leaflet">
			<Suspense fallback={locationLoader}>
				<GomuksLeaflet tileTemplate={tileTemplate} lat={lat} long={long} prec={prec} marker={marker}/>
			</Suspense>
		</div>
	} else if (mapProvider === "google") {
		const url = `https://www.google.com/maps/embed/v1/place?key=${GOOGLE_MAPS_API_KEY}&q=${lat},${long}`
		return <iframe className="location-container google" loading="lazy" referrerPolicy="no-referrer" src={url} />
	} else {
		return <div className="location-container blank">
			<a
				href={`https://www.openstreetmap.org/?mlat=${lat}&mlon=${long}`}
				target="_blank" rel="noreferrer noopener"
			>
				{marker}
			</a>
		</div>
	}
}

export default LocationMessageBody