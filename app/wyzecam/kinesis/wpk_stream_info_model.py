from typing import Dict, List, Optional

from pydantic import BaseModel, ConfigDict, Field


class IceServer(BaseModel):
    url: str
    username: str = ""
    credential: str = ""


class ParamsBean(BaseModel):
    signaling_url: str = ""
    auth_token: str = ""
    ice_servers: List[IceServer] = Field(default_factory=list)


class PropertyBean(BaseModel):
    property_data: Dict[str, int] = Field(default_factory=dict, alias="property")


class Stream(BaseModel):
    property: PropertyBean
    device_id: str
    provider: str
    params: ParamsBean


class WpkStreamInfo(BaseModel):
    code: str
    ts: int
    msg: str
    data: List[Stream]
    traceId: Optional[str] = None

    model_config = ConfigDict(validate_by_name=True)
