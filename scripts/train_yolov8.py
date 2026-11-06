"""Train the small detector used by the Luckfox security prototype."""

from __future__ import annotations

import argparse
from pathlib import Path


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--data", default="configs/data.yaml")
    parser.add_argument("--model", default="yolov8n.pt")
    parser.add_argument("--imgsz", type=int, default=640)
    parser.add_argument("--epochs", type=int, default=100)
    parser.add_argument("--batch", type=int, default=-1)
    parser.add_argument("--device", default="0")
    parser.add_argument("--project", default="runs/detect")
    parser.add_argument("--name", default="luckfox_security_yolov8n")
    args = parser.parse_args()

    data = Path(args.data)
    if not data.exists():
        raise SystemExit(f"Dataset config not found: {data}")

    try:
        from ultralytics import YOLO
    except ImportError as exc:
        raise SystemExit("Install dependencies first: pip install ultralytics pyyaml") from exc

    model = YOLO(args.model)
    model.train(
        data=str(data),
        imgsz=args.imgsz,
        epochs=args.epochs,
        batch=args.batch,
        device=args.device,
        project=args.project,
        name=args.name,
        pretrained=True,
        plots=True,
    )


if __name__ == "__main__":
    main()
