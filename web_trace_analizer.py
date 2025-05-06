import os
import json
from flask import Flask, render_template, request

app = Flask(__name__)

# Set the path for trace storage folders
TRACE_STORAGE_PATH = "./trace-storage"
INDEX_FILE = os.path.join(TRACE_STORAGE_PATH, "index/traces.json")
TRACES_FOLDER = os.path.join(TRACE_STORAGE_PATH, "traces")


def load_traces_index():
    """Load traces index file."""
    with open(INDEX_FILE, "r") as file:
        return json.load(file)["traces"]


def load_trace_details(trace_id):
    """Load details of a specific trace based on its trace_id."""
    trace_file_path = os.path.join(TRACES_FOLDER, f"{trace_id}.json")
    if os.path.exists(trace_file_path):
        with open(trace_file_path, "r") as file:
            return json.load(file)
    return None


@app.route("/")
def traces_interface():
    """Render the traces interface."""
    service_filter = request.args.get("service", "").strip()
    operation_filter = request.args.get("operation", "").strip()
    status_filter = request.args.get("status", "").strip()

    traces = load_traces_index()
    filtered_traces = []

    for trace in traces:
        trace_id = trace["trace_id"]
        trace_details = load_trace_details(trace_id)

        if not trace_details:
            continue

        root_service = trace_details["spans"][0]["service_name"] if trace_details["spans"] else ""
        root_operation = trace_details["spans"][0]["operation_name"] if trace_details["spans"] else ""
        services_count = len(set(span["service_name"] for span in trace_details["spans"]))

        # Apply filters
        if service_filter and root_service != service_filter:
            continue
        if operation_filter and root_operation != operation_filter:
            continue
        if status_filter and str(trace["status"]) != status_filter:
            continue

        filtered_traces.append({
            "trace_id": trace_id,
            "root_operation": root_operation,
            "services_count": services_count,
            "duration": f"{(trace_details['end_time']['nanos'] - trace_details['spans'][0]['start_time']['nanos']) / 1e6} ms" if trace_details.get("end_time") else "N/A",
            "start_time": trace_details["spans"][0]["start_time"]["seconds"] if trace_details["spans"] else "N/A",
            "status": "Success" if trace["status"] == 1 else "Failed"
        })

    return render_template("traces_web.html", traces=filtered_traces)


if __name__ == "__main__":
    app.run(debug=True)