FROM python:3.11-slim
WORKDIR /srv
# LightGBM's wheel links against OpenMP at runtime; libgomp1 is the one system dep it needs.
RUN apt-get update && apt-get install -y --no-install-recommends libgomp1 \
    && rm -rf /var/lib/apt/lists/*
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY app ./app
COPY migrations ./migrations
ENV PORT=8003 MODELS_DIR=/data/models
EXPOSE 8003
CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8003"]
