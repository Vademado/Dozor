"""PC reference pipeline for validating detector + tracker + event rules.

This is intentionally separate from the RV1106 runtime. It is used to tune
thresholds and inspect event logs before porting the post-processing to C++.
"""

from __future__ import annotations

import argparse
import json
import math
import time
from collections import deque
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path


@dataclass
class TrackState:
    first_seen: float
    last_seen: float
    points: deque[tuple[float, float]] = field(default_factory=lambda: deque(maxlen=30))
    zone_since: float | None = None
    dwell_alerted: bool = False
    approach_alerted: bool = False


def center(box: list[float]) -> tuple[float, float]:
    x1, y1, x2, y2 = box
    return ((x1 + x2) / 2.0, (y1 + y2) / 2.0)


def point_in_rect(point: tuple[float, float], rect: tuple[float, float, float, float]) -> bool:
    x, y = point
    x1, y1, x2, y2 = rect
    return x1 <= x <= x2 and y1 <= y <= y2


def speed(points: deque[tuple[float, float]], dt: float) -> float:
    if len(points) < 2 or dt <= 0:
        return 0.0
    x1, y1 = points[-2]
    x2, y2 = points[-1]
    return math.hypot(x2 - x1, y2 - y1) / dt


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("video")
    parser.add_argument("--model", default="yolov8n.pt")
    parser.add_argument("--dwell-seconds", type=float, default=30.0)
    parser.add_argument("--zone", nargs=4, type=float, metavar=("X1", "Y1", "X2", "Y2"), default=(0, 0, 1, 1))
    parser.add_argument("--vehicle-roi", nargs=4, type=float, metavar=("X1", "Y1", "X2", "Y2"), default=(0, 0, 1, 1))
    parser.add_argument("--log", default="events.jsonl")
    args = parser.parse_args()

    try:
        import cv2
        from ultralytics import YOLO
    except ImportError as exc:
        raise SystemExit("Install dependencies first: pip install ultralytics opencv-python") from exc

    capture = cv2.VideoCapture(args.video)
    if not capture.isOpened():
        raise SystemExit(f"Cannot open video: {args.video}")

    model = YOLO(args.model)
    tracks: dict[int, TrackState] = {}
    last_time = time.monotonic()
    zone = tuple(args.zone)
    vehicle_roi = tuple(args.vehicle_roi)
    log_path = Path(args.log)
    vehicle_missing_frames = 0
    vehicle_missing_alerted = False
    last_events: dict[tuple[str, int | None], float] = {}

    def emit(event_type: str, track_id: int | None, reason: str) -> None:
        key = (event_type, track_id)
        if now - last_events.get(key, float("-inf")) < 10.0:
            return
        last_events[key] = now
        event = {
            "event_id": f"{int(time.time() * 1000)}-{event_type}",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "type": event_type,
            "track_id": track_id,
            "reason": reason,
        }
        with log_path.open("a", encoding="utf-8") as stream:
            stream.write(json.dumps(event, ensure_ascii=False) + "\n")
        print(json.dumps(event, ensure_ascii=False))

    while True:
        ok, frame = capture.read()
        if not ok:
            break
        now = time.monotonic()
        dt = max(now - last_time, 1e-3)
        last_time = now

        result = model.track(frame, persist=True, tracker="bytetrack.yaml", verbose=False)[0]
        boxes = result.boxes
        vehicle_present = False
        if boxes is not None and boxes.id is not None:
            for box, cls, track_id in zip(boxes.xyxy.tolist(), boxes.cls.tolist(), boxes.id.tolist()):
                label = model.names[int(cls)]
                point = center(box)
                if label in {"vehicle", "car"} and point_in_rect(point, vehicle_roi):
                    vehicle_present = True
                if label != "person":
                    continue
                track_id = int(track_id)
                state = tracks.setdefault(track_id, TrackState(first_seen=now, last_seen=now))
                state.last_seen = now
                state.points.append(point)
                in_zone = point_in_rect(point, zone)
                if in_zone and state.zone_since is None:
                    state.zone_since = now
                    state.dwell_alerted = False
                    state.approach_alerted = False
                elif not in_zone:
                    state.zone_since = None
                    state.dwell_alerted = False
                    state.approach_alerted = False
                if (
                    in_zone
                    and state.zone_since is not None
                    and now - state.zone_since >= args.dwell_seconds
                    and not state.dwell_alerted
                ):
                    state.dwell_alerted = True
                    emit("DWELL_TIMEOUT", track_id, f"zone dwell >= {args.dwell_seconds:.1f}s")
                if in_zone and point_in_rect(point, vehicle_roi) and not state.approach_alerted:
                    state.approach_alerted = True
                    emit("APPROACH_VEHICLE", track_id, "person entered vehicle ROI")
                if speed(state.points, dt) > 250.0:
                    emit("SUSPICIOUS_MOTION", track_id, "large inter-frame displacement")

        stale = [track_id for track_id, state in tracks.items() if now - state.last_seen > 2.0]
        for track_id in stale:
            del tracks[track_id]

        if vehicle_present:
            vehicle_missing_frames = 0
            vehicle_missing_alerted = False
        else:
            vehicle_missing_frames += 1
            if vehicle_missing_frames >= 5 and not vehicle_missing_alerted:
                vehicle_missing_alerted = True
                recent_track = max(tracks.items(), key=lambda item: item[1].last_seen, default=(None, None))[0]
                emit("VEHICLE_MISSING", recent_track, "vehicle absent in five consecutive frames")

        cv2.imshow("Luckfox security prototype", result.plot())
        if cv2.waitKey(1) & 0xFF == ord("q"):
            break

    capture.release()
    cv2.destroyAllWindows()


if __name__ == "__main__":
    main()
