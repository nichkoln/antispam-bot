
FROM python:3.9-slim


RUN apt-get update && apt-get install -y \
    build-essential \
    git-lfs \
    && git lfs install \
    && apt-get clean


WORKDIR /app


COPY requirements.txt .


RUN pip install --no-cache-dir -r requirements.txt


COPY . .


EXPOSE 8001


CMD ["uvicorn", "pyModelFastAPI:app", "--host", "0.0.0.0", "--port", "8001"]
