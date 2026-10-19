# Base image lives in the private ECR; run `aws ecr get-login-password --region us-west-2 | docker login --username AWS --password-stdin 134622832812.dkr.ecr.us-west-2.amazonaws.com` before building.
FROM 134622832812.dkr.ecr.us-west-2.amazonaws.com/netmind_inference_public:frps

COPY ./release/frps_linux_amd64 /usr/bin/frps
